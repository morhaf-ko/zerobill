// Package setup reads an owner's repositories, workflows, runners and variables
// so zerobill can write a starting config and check an existing one.
package setup

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/morhaf-ko/zerobill/internal/github"
	"gopkg.in/yaml.v3"
)

// RunsOn is one runs-on declaration in a workflow file.
type RunsOn struct {
	Path string
	Line int
	// Value is the declaration as written, lists joined with ", ".
	Value string
	// Labels are literal runner labels; Vars are repository variables it reads.
	Labels []string
	Vars   []string
}

var varRef = regexp.MustCompile(`vars\.([A-Za-z0-9_]+)`)

// ParseRunsOn returns the runs-on declaration of every job in a workflow file. A
// runner group or label map is reported with its keys in Value and no Labels.
func ParseRunsOn(path, text string) ([]RunsOn, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	var out []RunsOn
	jobs := child(doc.Content[0], "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 1; i < len(jobs.Content); i += 2 {
		job := jobs.Content[i]
		if job.Kind != yaml.MappingNode {
			continue
		}
		for k := 0; k < len(job.Content); k += 2 {
			if job.Content[k].Value == "runs-on" {
				out = append(out, runsOn(path, job.Content[k].Line, job.Content[k+1]))
			}
		}
	}
	return out, nil
}

func child(n *yaml.Node, key string) *yaml.Node {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func runsOn(path string, line int, v *yaml.Node) RunsOn {
	r := RunsOn{Path: path, Line: line}
	var parts []string
	switch v.Kind {
	case yaml.ScalarNode:
		parts = []string{v.Value}
		r.Labels = literal(v.Value)
	case yaml.SequenceNode:
		for _, it := range v.Content {
			parts = append(parts, it.Value)
			r.Labels = append(r.Labels, literal(it.Value)...)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(v.Content); i += 2 {
			parts = append(parts, v.Content[i].Value+": "+v.Content[i+1].Value)
		}
	}
	r.Value = strings.Join(parts, ", ")
	for _, m := range varRef.FindAllStringSubmatch(r.Value, -1) {
		r.Vars = append(r.Vars, m[1])
	}
	return r
}

// literal returns s as a runner label unless it is an expression.
func literal(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || strings.Contains(s, "${{") {
		return nil
	}
	return []string{s}
}

// Workflows returns the runs-on declarations of every workflow in repo. A
// repository without workflows returns none.
func Workflows(gh *github.Client, repo string) ([]RunsOn, error) {
	var files []struct {
		Name, Path, Type string
	}
	err := gh.Get("/repos/"+repo+"/contents/.github/workflows", &files)
	if github.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []RunsOn
	for _, f := range files {
		if f.Type != "file" || !(strings.HasSuffix(f.Name, ".yml") || strings.HasSuffix(f.Name, ".yaml")) {
			continue
		}
		var file struct {
			Content, Encoding string
		}
		if err := gh.Get("/repos/"+repo+"/contents/"+escapePath(f.Path), &file); err != nil {
			return nil, err
		}
		if file.Encoding != "base64" {
			return nil, fmt.Errorf("%s/%s: unexpected encoding %q", repo, f.Path, file.Encoding)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
		if err != nil {
			return nil, fmt.Errorf("%s/%s: %w", repo, f.Path, err)
		}
		decls, err := ParseRunsOn(f.Path, string(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", repo, err)
		}
		out = append(out, decls...)
	}
	return out, nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// Variables returns a repository's Actions variables by name.
func Variables(gh *github.Client, repo string) (map[string]string, error) {
	out := map[string]string{}
	err := gh.Paginate("/repos/"+repo+"/actions/variables?per_page=30", func(body []byte) error {
		var page struct {
			Variables []struct{ Name, Value string } `json:"variables"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		for _, v := range page.Variables {
			out[v.Name] = v.Value
		}
		return nil
	})
	return out, err
}

// Runner is a registered self-hosted runner.
type Runner struct {
	Online bool
	// All holds every label as GitHub reports it; Labels leaves out GitHub's
	// defaults such as self-hosted, Linux and X64 and key=value metadata labels.
	All    []string
	Labels []string
	// Managed is set for runners a manager creates per job, recognised by GARM's
	// runner-pool-id metadata label.
	Managed bool
}

// defaultLabels are the labels GitHub adds to every self-hosted runner.
var defaultLabels = []string{"self-hosted", "linux", "windows", "macos", "x64", "arm", "arm64"}

// IsDefaultLabel reports whether label is one GitHub adds to every self-hosted
// runner, or key=value metadata, so it cannot name one pool.
func IsDefaultLabel(label string) bool {
	return strings.Contains(label, "=") || slices.Contains(defaultLabels, strings.ToLower(label))
}

// Runners lists self-hosted runners at paths, organization or repository level.
func Runners(gh *github.Client, paths []string) ([]Runner, error) {
	var out []Runner
	for _, p := range paths {
		err := gh.Paginate(p, func(body []byte) error {
			var page struct {
				Runners []struct {
					Status string `json:"status"`
					Labels []struct {
						Name string `json:"name"`
					} `json:"labels"`
				} `json:"runners"`
			}
			if err := json.Unmarshal(body, &page); err != nil {
				return err
			}
			for _, r := range page.Runners {
				rr := Runner{Online: r.Status == "online"}
				for _, l := range r.Labels {
					rr.All = append(rr.All, l.Name)
					switch {
					case strings.HasPrefix(l.Name, "runner-pool-id="):
						rr.Managed = true
					case !IsDefaultLabel(l.Name):
						rr.Labels = append(rr.Labels, l.Name)
					}
				}
				out = append(out, rr)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// RunnerPaths returns where an owner's runners are listed: once for an
// organization, per repository for a user.
func RunnerPaths(owner string, org bool, repos []string) []string {
	if org {
		return []string{"/orgs/" + url.PathEscape(owner) + "/actions/runners?per_page=100"}
	}
	paths := make([]string, len(repos))
	for i, r := range repos {
		paths[i] = "/repos/" + r + "/actions/runners?per_page=100"
	}
	return paths
}

// PrivateRepos lists the owner's private, non-archived repositories, sorted.
// A user owner must be the token's user.
func PrivateRepos(gh *github.Client, owner string, org bool) ([]string, error) {
	path := "/orgs/" + url.PathEscape(owner) + "/repos?type=private&per_page=100"
	if !org {
		path = "/user/repos?visibility=private&affiliation=owner&per_page=100"
	}
	var repos []string
	err := gh.Paginate(path, func(body []byte) error {
		var page []struct {
			FullName string `json:"full_name"`
			Archived bool   `json:"archived"`
			Private  bool   `json:"private"`
			Owner    struct {
				Login string `json:"login"`
			} `json:"owner"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		for _, r := range page {
			if r.Private && !r.Archived && strings.EqualFold(r.Owner.Login, owner) {
				repos = append(repos, r.FullName)
			}
		}
		return nil
	})
	sort.Strings(repos)
	return repos, err
}
