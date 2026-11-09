package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/config"
	"github.com/morhaf-ko/zerobill/internal/discover"
	"github.com/morhaf-ko/zerobill/internal/github"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

func TestCredentialSource(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	if got := credentialSource(); got != "GitHub CLI" {
		t.Fatal(got)
	}
	t.Setenv("GH_TOKEN", "do-not-print-gh")
	if got := credentialSource(); got != "GH_TOKEN" {
		t.Fatal(got)
	}
	t.Setenv("GITHUB_TOKEN", "do-not-print-github")
	if got := credentialSource(); got != "GITHUB_TOKEN" {
		t.Fatal(got)
	}
	if token, err := githubToken(); err != nil || token != "do-not-print-github" {
		t.Fatal("wrong credential precedence")
	}
	t.Setenv("GITHUB_TOKEN", "")
	if token, err := githubToken(); err != nil || token != "do-not-print-gh" {
		t.Fatal("GH_TOKEN not used")
	}
	for _, source := range []string{"GITHUB_TOKEN", "GH_TOKEN", "GitHub CLI"} {
		text := accessSteps(source, true)
		if strings.Contains(text, "do-not-print") {
			t.Fatal("credential leaked")
		}
		if source != "GitHub CLI" && (strings.Contains(text, "gh auth refresh") || !strings.Contains(text, "env -u GITHUB_TOKEN -u GH_TOKEN zerobill status")) {
			t.Fatal(text)
		}
	}
}

func TestConnectionGuidance(t *testing.T) {
	for _, tt := range []struct {
		source       string
		code         int
		limited      bool
		want, absent string
	}{
		{"GitHub CLI", 401, false, "gh auth login", "--scopes"},
		{"GITHUB_TOKEN", 401, false, "env -u GITHUB_TOKEN", "gh auth refresh"},
		{"GitHub CLI", 403, true, "rate limiting requests", "gh auth refresh"},
		{"GitHub CLI", 404, false, "Check owner", "gh auth refresh"},
	} {
		err := connectionProblem("read owner", tt.source, "acme", true, &github.StatusError{Code: tt.code, RateLimited: tt.limited})
		if !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), tt.absent) {
			t.Fatal(err)
		}
	}
}

func TestShellQuote(t *testing.T) {
	text := "acme/repo'$(printf injected)"
	out, err := exec.Command("sh", "-c", "printf '%s' "+shellQuote(text)).Output()
	if err != nil || string(out) != text {
		t.Fatalf("%q: %v", out, err)
	}
}

func TestIncompleteStatus(t *testing.T) {
	a := &app{cfg: &config.Config{Owner: "acme", Blacksmith: true}, authSource: "GITHUB_TOKEN", login: "ada", org: true}
	a.providers, a.routes = build(a.cfg, 2000)
	for _, tt := range []struct {
		name   string
		found  discover.Result
		errs   map[string]string
		access bool
		want   string
	}{
		{"denied", discover.Result{PrivateRepos: 1, Repos: []discover.Repo{{Name: "acme/app"}}}, map[string]string{"runner": "403 Forbidden"}, true, "env -u GITHUB_TOKEN -u GH_TOKEN zerobill status"},
		{"no visibility", discover.Result{}, nil, false, "none visible"},
		{"workflow setup", discover.Result{PrivateRepos: 1, Fixed: []string{"acme/app .github/workflows/ci.yml:3: ubuntu-latest"}}, nil, false, "gh variable set CI_RUNNER --repo 'acme/app' --body ubuntu-latest"},
		{"partial workflows", discover.Result{PrivateRepos: 1, Errors: []string{"acme/app: invalid workflow"}}, nil, false, "some could not be read"},
		{"billing unavailable", discover.Result{PrivateRepos: 1, Repos: []discover.Repo{{Name: "acme/app"}}}, map[string]string{"github": "404 Not Found"}, false, "https://github.com/organizations/acme/settings/billing/usage"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := render(&out, now, a, nil, map[string]policy.State{"blacksmith": {}}, tt.found, collect.Result{Errors: tt.errs, AccessProblem: tt.access})
			if !errors.Is(err, errStatusIncomplete) {
				t.Fatalf("status error: %v", err)
			}
			text := out.String()
			for _, want := range []string{"Status incomplete", "ada via GITHUB_TOKEN", "recommendations are withheld", tt.want} {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q: %s", want, text)
				}
			}
			for _, absent := range []string{"2700 free", "0 min", "VARIABLE", "zerobill run --apply"} {
				if strings.Contains(text, absent) {
					t.Fatalf("misleading output %q: %s", absent, text)
				}
			}
		})
	}
}

func TestConnectedStatusStopsBeforeUsageWithoutPrivateRepos(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch r.URL.Path {
		case "/user":
			fmt.Fprint(w, `{"login":"ada","plan":{"name":"pro"}}`)
		case "/users/acme":
			fmt.Fprint(w, `{"type":"Organization"}`)
		case "/orgs/acme":
			w.WriteHeader(403)
		case "/orgs/acme/repos":
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected cold scan: %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	gh := github.New("fixture-token")
	gh.BaseURL = srv.URL
	a := &app{cfg: &config.Config{Owner: "acme", Blacksmith: true, ReservePercent: 10}, gh: gh, stateDir: t.TempDir(), authSource: "GitHub CLI"}
	if err := a.connect(); err != nil {
		t.Fatal(err)
	}
	if a.login != "ada" || a.providers[1].Monthly != 2000 {
		t.Fatalf("identity or conservative org plan: %+v", a)
	}
	var out bytes.Buffer
	if err := reportStatus(&out, a); !errors.Is(err, errStatusIncomplete) {
		t.Fatal(err)
	}
	if hits != 4 || !strings.Contains(out.String(), "gh repo list 'acme' --visibility private") {
		t.Fatalf("hits %d: %s", hits, out.String())
	}
}

func TestStatusChecksAccessBeforeColdScan(t *testing.T) {
	for _, denied := range []bool{false, true} {
		var paths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			switch r.URL.Path {
			case "/organizations/acme/settings/billing/usage":
				if denied {
					w.WriteHeader(404)
					fmt.Fprint(w, `{"message":"Not Found"}`)
					return
				}
				fmt.Fprint(w, `{"usageItems":[]}`)
			case "/orgs/acme/actions/runners":
				fmt.Fprint(w, `{"runners":[]}`)
			case "/orgs/acme/repos":
				fmt.Fprint(w, `[{"full_name":"acme/app","private":true,"owner":{"login":"acme"}}]`)
			case "/repos/acme/app/actions/runs":
				fmt.Fprint(w, `{"total_count":0,"workflow_runs":[]}`)
			default:
				t.Errorf("unexpected request %s", r.URL.Path)
				w.WriteHeader(500)
			}
		}))
		gh := github.New("fixture-token")
		gh.BaseURL = srv.URL
		a := &app{cfg: &config.Config{Owner: "acme", Runner: "home-ci", Blacksmith: true}, col: &collect.Collector{GH: gh, Owner: "acme", Org: true}}
		a.providers, a.routes = build(a.cfg, 2000)
		var progress bytes.Buffer
		r := statusObservations(a, &progress)
		srv.Close()
		if denied {
			if len(paths) != 1 || r.Errors["github"] == "" || progress.Len() != 0 {
				t.Fatalf("did not fail promptly: %v %+v %q", paths, r, progress.String())
			}
		} else {
			if len(r.States) != 3 || len(r.Errors) != 0 || len(paths) < 3 || paths[1] != "/orgs/acme/actions/runners" || !strings.Contains(progress.String(), "no Blacksmith API key needed") {
				t.Fatalf("wrong read order: %v %+v %q", paths, r, progress.String())
			}
		}
	}
}

func TestStatusRateLimitDoesNotRequestMoreScopes(t *testing.T) {
	a := &app{cfg: &config.Config{Owner: "acme"}, login: "ada", authSource: "GitHub CLI"}
	var out bytes.Buffer
	err := render(&out, now, a, nil, nil, discover.Result{PrivateRepos: 1, Repos: []discover.Repo{{Name: "acme/app"}}}, collect.Result{Errors: map[string]string{"github": "secondary rate limit"}, RateLimited: true})
	if !errors.Is(err, errStatusIncomplete) || !strings.Contains(out.String(), "Wait until its rate limit resets") || strings.Contains(out.String(), "gh auth refresh") {
		t.Fatalf("wrong recovery: %v %s", err, out.String())
	}
}
