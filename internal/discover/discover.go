// Package discover finds the private repositories whose workflows read
// zerobill's runner variables, and the jobs still pinned to a fixed label.
package discover

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

// Variables zerobill sets: CI_RUNNER for everyone's jobs, BOT_RUNNER for checks
// dependency bots trigger.
var Variables = []string{"CI_RUNNER", "BOT_RUNNER"}

// Repo is a repository whose workflows read zerobill's variables.
type Repo struct {
	Name string
	// Vars are the zerobill variables its workflows read.
	Vars []string
}

// Result is what Discover found.
type Result struct {
	Repos []Repo
	// Fixed are runs-on declarations pinned to a literal label, which zerobill
	// cannot route, as "owner/name path:line: value".
	Fixed []string
	// Errors are repositories whose workflows could not be read.
	Errors []string
}

// Discover scans the owner's private repositories.
func Discover(gh *github.Client, owner string, org bool) (Result, error) {
	var r Result
	names, err := PrivateRepos(gh, owner, org)
	if err != nil {
		return r, err
	}
	for _, name := range names {
		decls, err := Workflows(gh, name)
		if err != nil {
			r.Errors = append(r.Errors, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		var vars []string
		for _, d := range decls {
			for _, v := range d.Vars {
				if slices.Contains(Variables, v) && !slices.Contains(vars, v) {
					vars = append(vars, v)
				}
			}
			if len(d.Vars) == 0 && len(d.Labels) > 0 {
				r.Fixed = append(r.Fixed, fmt.Sprintf("%s %s:%d: %s", name, d.Path, d.Line, d.Value))
			}
		}
		if len(vars) > 0 {
			slices.Sort(vars)
			r.Repos = append(r.Repos, Repo{Name: name, Vars: vars})
		}
	}
	return r, nil
}
