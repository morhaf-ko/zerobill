// Package discover finds the private repositories whose workflows read
// zerobill's runner variables, and the jobs still pinned to a fixed label.
package discover

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/morhaf-ko/zerobill/internal/github"
	"gopkg.in/yaml.v3"
)

// decl is one runs-on declaration in a workflow file.
type decl struct {
	Path string
	Line int
	// Value is the declaration as written, lists joined with ", ".
	Value string
	// Labels are literal runner labels; Vars are repository variables it reads.
	Labels []string
	Vars   []string
}

var varRef = regexp.MustCompile(`vars\.([A-Za-z0-9_]+)`)

// parseRunsOn returns the runs-on declaration of every job in a workflow file. A
// runner group or label map is reported with its keys in Value and no Labels.
func parseRunsOn(path, text string) ([]decl, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	var out []decl
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

func runsOn(path string, line int, v *yaml.Node) decl {
	r := decl{Path: path, Line: line}
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

type workflowKey struct{ repo, path string }

type workflowBlob struct {
	sha   string
	decls []decl
}

// Scanner reuses parsed workflows whose directory entries still name the same blob.
type Scanner struct {
	blobs   map[workflowKey]workflowBlob
	pending *scanProgress
}

type scanProgress struct {
	repos  []github.Repo
	cursor int
	used   map[workflowKey]workflowBlob
	result Result
}

func (s *Scanner) workflows(gh *github.Client, repo string, next map[workflowKey]workflowBlob) ([]decl, error) {
	var files []struct {
		Name, Path, Type, SHA string
	}
	err := gh.ConditionalGet("/repos/"+repo+"/contents/.github/workflows", &files)
	if github.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []decl
	for _, f := range files {
		if f.Type != "file" || !(strings.HasSuffix(f.Name, ".yml") || strings.HasSuffix(f.Name, ".yaml")) {
			continue
		}
		key := workflowKey{repo, f.Path}
		blob, ok := s.blobs[key]
		if !ok || f.SHA == "" || blob.sha != f.SHA {
			raw, sha, err := readWorkflowFile(gh, repo, f.Path, "")
			if err != nil {
				return nil, err
			}
			if sha != "" && f.SHA != "" && sha != f.SHA {
				return nil, fmt.Errorf("%s/%s: workflow changed during discovery", repo, f.Path)
			}
			decls, err := parseRunsOn(f.Path, raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", repo, err)
			}
			blob = workflowBlob{sha: sha, decls: decls}
		}
		if blob.sha != "" && blob.sha == f.SHA {
			if s.blobs == nil {
				s.blobs = make(map[workflowKey]workflowBlob)
			}
			s.blobs[key] = blob
			next[key] = blob
		}
		out = append(out, blob.decls...)
	}
	return out, nil
}

// ErrWorkflowRead identifies a transient workflow read failure.
var ErrWorkflowRead = errors.New("workflow read failed")

func readWorkflowFile(gh *github.Client, repo, path, ref string) (string, string, error) {
	endpoint := "/repos/" + repo + "/contents/" + escapePath(path)
	if ref != "" {
		endpoint += "?ref=" + url.QueryEscape(ref)
	}
	var file struct {
		Content, Encoding, SHA string
	}
	if err := gh.Get(endpoint, &file); err != nil {
		var status *github.StatusError
		if errors.As(err, &status) && status.Code < 500 &&
			status.Code != http.StatusRequestTimeout && status.Code != http.StatusTooManyRequests && !github.IsRateLimited(err) {
			return "", "", err
		}
		return "", "", fmt.Errorf("%w: %w", ErrWorkflowRead, err)
	}
	if file.Encoding != "base64" {
		return "", "", fmt.Errorf("%s/%s: unexpected encoding %q", repo, path, file.Encoding)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return "", "", fmt.Errorf("%s/%s: %w", repo, path, err)
	}
	return string(raw), file.SHA, nil
}

var commitSHA = regexp.MustCompile(`^[[:xdigit:]]{40}$`)
var workflowPath = regexp.MustCompile(`^\.github/workflows/[^/@]+\.ya?ml$`)
var directVariable = regexp.MustCompile(`^\$\{\{\s*vars\.(CI_RUNNER|BOT_RUNNER)\s*\}\}$`)

type JobRoute struct {
	Variable  string
	Dependent string
}

// JobRoutes resolves static GitHub job names to single repository-variable labels.
func JobRoutes(gh *github.Client, repo, path, sha, event string) (map[string]JobRoute, error) {
	if (event != "push" && event != "workflow_dispatch") || !commitSHA.MatchString(sha) || !workflowPath.MatchString(path) {
		return nil, fmt.Errorf("workflow revision cannot be verified for event %q", event)
	}
	raw, _, err := readWorkflowFile(gh, repo, path, sha)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("workflow is empty")
	}
	jobs := child(doc.Content[0], "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("workflow jobs cannot be verified")
	}
	routes := map[string]JobRoute{}
	names := map[string]string{}
	dependents := map[string]string{}
	for i := 0; i < len(jobs.Content); i += 2 {
		id, job := jobs.Content[i].Value, jobs.Content[i+1]
		if job.Kind != yaml.MappingNode || child(job, "<<") != nil || child(job, "uses") != nil || child(job, "strategy") != nil {
			return nil, fmt.Errorf("job %q has an unsupported declaration for recovery", id)
		}
		name := id
		if n := child(job, "name"); n != nil {
			if n.Kind != yaml.ScalarNode || n.Tag != "!!str" || n.Value == "" || strings.Contains(n.Value, "${{") {
				return nil, fmt.Errorf("job %q has an unresolved name", id)
			}
			name = n.Value
		}
		if _, exists := routes[name]; exists {
			return nil, fmt.Errorf("workflow has duplicate job name %q", name)
		}
		names[id] = name
		var route JobRoute
		if n := child(job, "runs-on"); n != nil && n.Kind == yaml.ScalarNode {
			if match := directVariable.FindStringSubmatch(n.Value); match != nil {
				route.Variable = match[1]
			}
		}
		routes[name] = route
		if n := child(job, "needs"); n != nil {
			nodes := n.Content
			if n.Kind == yaml.ScalarNode {
				nodes = []*yaml.Node{n}
			}
			if n.Kind != yaml.SequenceNode && n.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("job %q has unresolved dependencies", name)
			}
			for _, need := range nodes {
				if need.Kind != yaml.ScalarNode || need.Tag != "!!str" || strings.Contains(need.Value, "${{") {
					return nil, fmt.Errorf("job %q has unresolved dependencies", name)
				}
				dependents[need.Value] = name
			}
		}
	}
	for id, dependent := range dependents {
		name, exists := names[id]
		if !exists {
			return nil, fmt.Errorf("job %q has an unknown dependency %q", dependent, id)
		}
		route := routes[name]
		route.Dependent = dependent
		routes[name] = route
	}
	return routes, nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
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
	Repos         []Repo
	PrivateRepos  int
	Scanned       int
	ExampleRepo   string
	AccessProblem bool
	RateLimited   bool
	// Fixed are runs-on declarations pinned to a literal label, which zerobill
	// cannot route, as "owner/name path:line: value".
	Fixed []string
	// Errors are repositories whose workflows could not be read.
	Errors []string
}

// Discover scans the owner's private repositories, sorted by name.
func Discover(gh *github.Client, owner string, org bool) (Result, error) {
	repos, err := gh.Repos(owner, org)
	if err != nil {
		return Result{}, err
	}
	return (&Scanner{}).Scan(gh, repos)
}

// Scan discovers workflow routing in the supplied owner inventory.
func (s *Scanner) Scan(gh *github.Client, repos []github.Repo) (Result, error) {
	repos = slices.Clone(repos)
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })
	if s.pending == nil || !slices.Equal(repos, s.pending.repos) {
		s.pending = &scanProgress{repos: repos, used: make(map[workflowKey]workflowBlob)}
		for _, repo := range repos {
			if repo.Private {
				s.pending.result.PrivateRepos++
			}
		}
	}
	progress := s.pending
	r := &progress.result
	for ; progress.cursor < len(repos); progress.cursor++ {
		repo := repos[progress.cursor]
		if !repo.Private {
			continue
		}
		if r.ExampleRepo == "" {
			r.ExampleRepo = repo.Name
		}
		name := repo.Name
		decls, err := s.workflows(gh, name, progress.used)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || github.IsRateLimited(err) {
				incomplete := *r
				incomplete.RateLimited = github.IsRateLimited(err)
				incomplete.Errors = append(slices.Clone(r.Errors), fmt.Sprintf("%s: %v", name, err))
				if incomplete.RateLimited {
					return incomplete, err
				}
				return incomplete, fmt.Errorf("workflow scan timed out at %s: %w", name, err)
			}
			r.Errors = append(r.Errors, fmt.Sprintf("%s: %v", name, err))
			r.AccessProblem = r.AccessProblem || github.IsAccessDenied(err)
			continue
		}
		r.Scanned++
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
	s.blobs = progress.used
	s.pending = nil
	return *r, nil
}
