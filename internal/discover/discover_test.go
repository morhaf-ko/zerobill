package discover

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/morhaf-ko/zerobill/internal/github"
)

const workflow = `name: ci
on: [pull_request]
jobs:
  inline:
    runs-on: ubuntu-latest # comment
  variable:
    runs-on: ${{ vars.CI_RUNNER }}
  bot:
    runs-on: ${{ github.actor == 'renovate[bot]' && vars.BOT_RUNNER || vars.CI_RUNNER }}
  flow:
    runs-on: [self-hosted,
      home-ci]
  block:
    runs-on:
      # the GPU pool
      - self-hosted
      - gpu
    steps:
      - run: echo
  folded:
    runs-on: >-
      ${{ vars.LIGHT_RUNNER }}
  group:
    runs-on:
      group: big
`

func TestParseRunsOn(t *testing.T) {
	got, err := parseRunsOn("ci.yml", workflow)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		line   int
		labels []string
		vars   []string
	}{
		{5, []string{"ubuntu-latest"}, nil},
		{7, nil, []string{"CI_RUNNER"}},
		{9, nil, []string{"BOT_RUNNER", "CI_RUNNER"}},
		{11, []string{"self-hosted", "home-ci"}, nil},
		{14, []string{"self-hosted", "gpu"}, nil},
		{21, nil, []string{"LIGHT_RUNNER"}},
		{24, nil, nil},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d declarations: %+v", len(got), got)
	}
	for i, w := range want {
		g := got[i]
		if g.Line != w.line || !slices.Equal(g.Labels, w.labels) || !slices.Equal(g.Vars, w.vars) {
			t.Errorf("declaration %d: got line %d labels %v vars %v (%q)", i, g.Line, g.Labels, g.Vars, g.Value)
		}
	}
	if got[6].Value != "group: big" {
		t.Errorf("group value %q", got[6].Value)
	}
	if _, err := parseRunsOn("bad.yml", "jobs: [unclosed"); err == nil {
		t.Error("invalid YAML parsed without an error")
	}
}

func TestDiscover(t *testing.T) {
	files := map[string]string{
		"acme/app":  "jobs:\n  ci:\n    runs-on: ${{ vars.CI_RUNNER }}\n  lint:\n    runs-on: ubuntu-latest\n",
		"acme/bots": "jobs:\n  ci:\n    runs-on: ${{ github.actor == 'renovate[bot]' && vars.BOT_RUNNER || vars.CI_RUNNER }}\n",
		"acme/docs": "jobs:\n  build:\n    runs-on: ubuntu-24.04\n",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/orgs/acme/repos":
			fmt.Fprint(w, `[{"full_name":"acme/app","private":true,"owner":{"login":"acme"}},{"full_name":"acme/bots","private":true,"owner":{"login":"acme"}},
				{"full_name":"acme/docs","private":true,"owner":{"login":"acme"}},{"full_name":"acme/old","private":true,"archived":true,"owner":{"login":"acme"}},
				{"full_name":"acme/empty","private":true,"owner":{"login":"acme"}}]`)
		case strings.HasSuffix(r.URL.Path, "/contents/.github/workflows"):
			repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/contents/.github/workflows")
			if _, ok := files[repo]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprint(w, `[{"name":"ci.yml","path":".github/workflows/ci.yml","type":"file"}]`)
		case strings.HasSuffix(r.URL.Path, "/contents/.github/workflows/ci.yml"):
			repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/contents/.github/workflows/ci.yml")
			fmt.Fprintf(w, `{"encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte(files[repo])))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	gh := github.New("t")
	gh.BaseURL = srv.URL
	got, err := Discover(gh, "acme", true)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExampleRepo != "acme/app" || got.PrivateRepos != 4 || len(got.Repos) != 2 || got.Repos[0].Name != "acme/app" || !slices.Equal(got.Repos[0].Vars, []string{"CI_RUNNER"}) ||
		!slices.Equal(got.Repos[1].Vars, []string{"BOT_RUNNER", "CI_RUNNER"}) {
		t.Fatalf("repos %+v", got.Repos)
	}
	want := []string{"acme/app .github/workflows/ci.yml:5: ubuntu-latest", "acme/docs .github/workflows/ci.yml:3: ubuntu-24.04"}
	if !slices.Equal(got.Fixed, want) || len(got.Errors) != 0 {
		t.Fatalf("fixed %q errors %v", got.Fixed, got.Errors)
	}
}
