package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/morhaf-ko/zerobill/internal/config"
	"github.com/morhaf-ko/zerobill/internal/discover"
	"github.com/morhaf-ko/zerobill/internal/github"
)

// The journey uses synthetic provider responses and a local webhook, never real credentials or jobs.
func TestSetupToRoutingJourney(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	var setup bytes.Buffer
	if err := initConfig(nil, strings.NewReader("acme\nhome-ci\ny\n"), &setup); err != nil {
		t.Fatal(err)
	}
	path, _ := config.Path()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := os.ReadFile(filepath.Join("..", "..", "examples", "acceptance.yml"))
	if err != nil {
		t.Fatal(err)
	}
	vars := &variables{values: map[string]string{}}
	online, denied := true, false
	used := 1600
	writes, warnings := 0, 0
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "monthly free allowance") {
			warnings++
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()
	cfg.Notify = hook.URL
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("GitHub request missing fixture credential")
		}
		if strings.Contains(r.URL.Path, "/actions/variables") {
			if r.Method != http.MethodGet {
				writes++
			}
			vars.ServeHTTP(w, r)
			return
		}
		switch r.URL.Path {
		case "/user":
			fmt.Fprint(w, `{"login":"ada","plan":{"name":"free"}}`)
		case "/users/acme":
			fmt.Fprint(w, `{"type":"Organization"}`)
		case "/orgs/acme":
			fmt.Fprint(w, `{"plan":{"name":"free"}}`)
		case "/orgs/acme/repos":
			fmt.Fprint(w, `[{"full_name":"acme/sandbox","private":true,"owner":{"login":"acme"}}]`)
		case "/repos/acme/sandbox/contents/.github/workflows":
			fmt.Fprint(w, `[{"name":"acceptance.yml","path":".github/workflows/acceptance.yml","type":"file"}]`)
		case "/repos/acme/sandbox/contents/.github/workflows/acceptance.yml":
			_ = json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString(workflow)})
		case "/organizations/acme/settings/billing/usage":
			if denied {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			fmt.Fprintf(w, `{"usageItems":[{"product":"actions","sku":"Actions Linux","quantity":%d,"unitType":"Minutes"}]}`, used)
		case "/orgs/acme/actions/runners":
			status := "offline"
			if online {
				status = "online"
			}
			fmt.Fprintf(w, `{"runners":[{"status":%q,"labels":[{"name":"home-ci"}]}]}`, status)
		case "/repos/acme/sandbox/actions/runs":
			if r.URL.Query().Get("status") != "" {
				fmt.Fprint(w, `{"workflow_runs":[]}`)
				return
			}
			fmt.Fprint(w, `{"workflow_runs":[{"id":1,"run_attempt":1,"status":"completed"}]}`)
		case "/repos/acme/sandbox/actions/runs/1/jobs":
			end := now.Add(-time.Hour)
			fmt.Fprintf(w, `{"jobs":[{"id":1,"status":"completed","runner_name":"synthetic","labels":["blacksmith-2vcpu-ubuntu-2404"],"started_at":%q,"completed_at":%q}]}`,
				end.Add(-2700*time.Minute).Format(time.RFC3339), end.Format(time.RFC3339))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	gh := github.New("fixture-token")
	gh.BaseURL = srv.URL
	a := &app{cfg: cfg, gh: gh, stateDir: t.TempDir(), authSource: "GitHub CLI"}
	if err := a.connect(); err != nil {
		t.Fatal(err)
	}
	a.col.Now = func() time.Time { return now }
	found, err := discover.Discover(gh, cfg.Owner, a.org)
	if err != nil || len(found.Repos) != 1 || len(found.Errors) != 0 {
		t.Fatalf("discovery: %+v %v", found, err)
	}
	a.col.Repositories = names(found.Repos)
	var report bytes.Buffer
	if err := reportStatus(&report, a); err != nil || !strings.Contains(report.String(), "CI_RUNNER  home-ci") {
		t.Fatalf("setup report: %v\n%s", err, report.String())
	}
	start := func(write bool) *controller {
		t.Helper()
		c, err := newController(a, write)
		if err != nil {
			t.Fatal(err)
		}
		c.repos = found.Repos
		return c
	}
	c := start(false)
	step(t, c, 0, a.col.Observe(a.providers))
	if writes != 0 || warnings != 0 {
		t.Fatal("preview wrote routing or delivered capacity warnings")
	}
	c = start(true)
	check := func(at time.Duration, want string) {
		t.Helper()
		l := step(t, c, at, a.col.Observe(a.providers))
		if len(l.Errors) != 0 || vars.values["acme/sandbox/CI_RUNNER"] != want {
			t.Fatalf("at %s: %+v variables %v", at, l, vars.values)
		}
	}
	check(0, "home-ci")
	if warnings != 2 {
		t.Fatalf("expected two provider warnings, got %d", warnings)
	}
	c = start(true)
	check(time.Minute, "home-ci")
	if warnings != 2 {
		t.Fatalf("restart duplicated capacity warnings: %d", warnings)
	}
	online = false
	check(2*time.Minute, githubLabel)
	used = 1800
	check(3*time.Minute, "home-ci")
	cfg.SelfHostedLabel = ""
	a.providers, a.routes = build(cfg, 2000)
	c = start(true)
	check(4*time.Minute, config.WaitingLabel)
	cfg.SelfHostedLabel, online = "home-ci", true
	a.providers, a.routes = build(cfg, 2000)
	c = start(true)
	check(5*time.Minute, "home-ci")
	denied = true
	before := writes
	l := step(t, c, 6*time.Minute, a.col.Observe(a.providers))
	if l.Errors["write"] == "" || writes != before || vars.values["acme/sandbox/CI_RUNNER"] != "home-ci" {
		t.Fatalf("failed reads changed routing: %+v", l)
	}
}
