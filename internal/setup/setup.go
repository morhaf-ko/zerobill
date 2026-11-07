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

var (
	runsOnKey = regexp.MustCompile(`^(\s*)runs-on:\s*(.*)$`)
	varRef    = regexp.MustCompile(`vars\.([A-Za-z0-9_]+)`)
)

// ParseRunsOn finds runs-on declarations in a workflow file: inline values, flow
// lists and block lists. A group or label map is reported with its keys as Value.
func ParseRunsOn(path, text string) []RunsOn {
	lines := strings.Split(text, "\n")
	var out []RunsOn
	for i := 0; i < len(lines); i++ {
		m := runsOnKey.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		indent, value := len(m[1]), stripComment(m[2])
		var items []string
		if value == "" {
			for j := i + 1; j < len(lines); j++ {
				l := lines[j]
				if strings.TrimSpace(l) == "" {
					continue
				}
				if leading(l) <= indent && !strings.HasPrefix(strings.TrimSpace(l), "- ") {
					break
				}
				if leading(l) < indent {
					break
				}
				items = append(items, strings.TrimPrefix(stripComment(strings.TrimSpace(l)), "- "))
			}
			value = strings.Join(items, ", ")
		} else if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
			for _, it := range strings.Split(strings.Trim(value, "[]"), ",") {
				items = append(items, strings.TrimSpace(it))
			}
		} else {
			items = []string{value}
		}
		r := RunsOn{Path: path, Line: i + 1, Value: value}
		for _, it := range items {
			it = strings.Trim(it, `"'`)
			for _, v := range varRef.FindAllStringSubmatch(it, -1) {
				r.Vars = append(r.Vars, v[1])
			}
			if it != "" && !strings.Contains(it, "${{") && !strings.Contains(it, ":") {
				r.Labels = append(r.Labels, it)
			}
		}
		out = append(out, r)
	}
	return out
}

func leading(s string) int { return len(s) - len(strings.TrimLeft(s, " \t")) }

func stripComment(s string) string {
	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
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
		out = append(out, ParseRunsOn(f.Path, string(raw))...)
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
	// Labels are the runner's routing labels; GitHub's defaults such as
	// self-hosted, Linux and X64 and key=value metadata labels are left out.
	Labels []string
	// Managed is set for runners a manager creates per job, recognised by GARM's
	// runner-pool-id metadata label.
	Managed bool
}

// defaultLabels are the labels GitHub adds to every self-hosted runner.
var defaultLabels = []string{"self-hosted", "linux", "windows", "macos", "x64", "arm", "arm64"}

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
					switch {
					case strings.HasPrefix(l.Name, "runner-pool-id="):
						rr.Managed = true
					case strings.Contains(l.Name, "="), slices.Contains(defaultLabels, strings.ToLower(l.Name)):
					default:
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
