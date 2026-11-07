package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/morhaf-ko/zerobill/internal/apply"
	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/github"
	"github.com/morhaf-ko/zerobill/internal/override"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

const (
	blacksmith = "blacksmith-2vcpu-ubuntu-2404"
	hosted     = "ubuntu-24.04"
)

// newController loads the example config, optionally in switch mode.
func newController(t *testing.T, mode string) *controller {
	t.Helper()
	path := filepath.Join(t.TempDir(), "zerobill.yaml")
	raw, err := os.ReadFile("../../examples/zerobill.yaml")
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte("mode: dry-run"), []byte("mode: "+mode), 1)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, providers, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return &controller{cfg: cfg, providers: providers, routes: cfg.PolicyRoutes(providers)}
}

func step(t *testing.T, c *controller, at time.Duration, r collect.Result) decisionLine {
	t.Helper()
	var buf bytes.Buffer
	if err := c.step(&buf, now.Add(at), r); err != nil {
		t.Fatal(err)
	}
	if bytes.Count(buf.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("want one line, got %q", buf.String())
	}
	var line decisionLine
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	return line
}

func route(t *testing.T, line decisionLine, name string) routeLine {
	t.Helper()
	for _, rl := range line.Routes {
		if rl.Name == name {
			return rl
		}
	}
	t.Fatalf("no route %s in %+v", name, line.Routes)
	return routeLine{}
}

func TestDryRunLogsEveryRoute(t *testing.T) {
	c := newController(t, "dry-run")
	r := collect.Result{
		States: map[string]policy.State{"blacksmith": {Online: true, UsedMinutes: 1240}, "github": {Online: true, UsedMinutes: 310}},
		Errors: map[string]string{"home": "GET /orgs/your-org/actions/runners: 403 Forbidden"},
	}
	line := step(t, c, 0, r)
	ci, bot := route(t, line, "ci"), route(t, line, "bot")
	if ci.Label != hosted || bot.Label != hosted || ci.Written != nil || line.Errors["home"] == "" {
		t.Fatalf("unexpected line %+v", line)
	}
	if !strings.Contains(ci.Reason, "blacksmith 46%, github 17%") || len(bot.Variables) != 1 {
		t.Fatalf("unexpected routes %+v", line.Routes)
	}
	if *line.Observed["github"].UsedMinutes != 310 || line.Observed["home"].Online != nil {
		t.Fatalf("unexpected observations %+v", line.Observed)
	}
}

func TestOnDemandStallLatches(t *testing.T) {
	c := newController(t, "dry-run")
	metered := func(home policy.State) collect.Result {
		return collect.Result{States: map[string]policy.State{"home": home, "blacksmith": {UsedMinutes: 100}, "github": {UsedMinutes: 1000}}}
	}
	steps := []struct {
		name  string
		at    time.Duration
		home  policy.State
		label string
	}{
		{"no runner and no queue stays home", 0, policy.State{}, "home-ci"},
		{"queue stall leaves home", 5 * time.Minute, policy.State{OldestQueued: 11 * time.Minute}, blacksmith},
		{"drained queue keeps the stall", 30 * time.Minute, policy.State{}, blacksmith},
		{"retry after the stall returns home", 66 * time.Minute, policy.State{}, "home-ci"},
		{"second stall leaves again", 90 * time.Minute, policy.State{OldestQueued: 11 * time.Minute}, blacksmith},
		{"online runner clears the stall", 110 * time.Minute, policy.State{Online: true}, "home-ci"},
	}
	for _, s := range steps {
		ci := route(t, step(t, c, s.at, metered(s.home)), "ci")
		if ci.Label != s.label {
			t.Fatalf("%s: got %s (%s)", s.name, ci.Label, ci.Reason)
		}
	}
}

func TestDisabledProviderLeavesRotation(t *testing.T) {
	c := newController(t, "dry-run")
	c.overridesPath = filepath.Join(t.TempDir(), "overrides.json")
	if err := override.Save(c.overridesPath, override.Set{Disabled: map[string]override.Entry{"home": {Since: now, Until: now.Add(time.Hour)}}}); err != nil {
		t.Fatal(err)
	}
	r := collect.Result{States: map[string]policy.State{"home": {Online: true}, "blacksmith": {UsedMinutes: 100}, "github": {UsedMinutes: 1000}}}
	line := step(t, c, 0, r)
	if route(t, line, "ci").Label != blacksmith || !strings.Contains(line.Disabled["home"], "until 2026-11-07T13:00:00Z") {
		t.Fatalf("unexpected line %+v", line)
	}
	if got := route(t, step(t, c, 2*time.Hour, r), "ci"); got.Label != "home-ci" {
		t.Fatalf("expired override still applies: %+v", got)
	}
}

func TestDisableAndEnable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overrides.json")
	if err := disable([]string{"home", "--for", "72h", "--config", "../../examples/zerobill.yaml", "--overrides", path}, now); err != nil {
		t.Fatal(err)
	}
	set, err := override.Load(path)
	if err != nil || !set.Disabled["home"].Until.Equal(now.Add(72*time.Hour)) {
		t.Fatalf("got %+v %v", set, err)
	}
	if err := disable([]string{"--config", "../../examples/zerobill.yaml", "--overrides", path, "pi"}, now); err == nil {
		t.Fatal("expected an error for an unknown provider")
	}
	if err := enable([]string{"home", "--overrides", path}); err != nil {
		t.Fatal(err)
	}
	if set, err := override.Load(path); err != nil || len(set.Disabled) != 0 {
		t.Fatalf("got %+v %v", set, err)
	}
}

// variables fakes the repository variables API and counts writes.
type variables struct {
	values map[string]string
	writes int
}

func (v *variables) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	switch r.Method {
	case http.MethodGet:
		val, ok := v.values[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"value":%q}`, val)
	default:
		v.writes++
		var in struct{ Name, Value string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		v.values[in.Name] = in.Value
		w.WriteHeader(http.StatusNoContent)
	}
}

func TestSwitchModeAppliesHoldsAndSkips(t *testing.T) {
	c := newController(t, "switch")
	vars := &variables{values: map[string]string{}}
	srv := httptest.NewServer(vars)
	defer srv.Close()
	gh := github.New("t")
	gh.BaseURL = srv.URL
	c.writer = &apply.Writer{GH: gh}
	c.appliedPath = filepath.Join(t.TempDir(), "applied.json")
	homeDown := map[string]policy.State{"home": {OldestQueued: 11 * time.Minute}, "blacksmith": {UsedMinutes: 100}, "github": {UsedMinutes: 1000}}
	homeUp := map[string]policy.State{"home": {Online: true}, "blacksmith": {UsedMinutes: 100}, "github": {UsedMinutes: 1000}}

	steps := []struct {
		name    string
		at      time.Duration
		result  collect.Result
		label   string
		held    bool
		written int
	}{
		{"home stall spills to blacksmith", 0, collect.Result{States: homeDown}, blacksmith, false, 4},
		{"unchanged decision writes nothing", time.Minute, collect.Result{States: homeDown}, blacksmith, false, 0},
		{"home back inside hold stays", 5 * time.Minute, collect.Result{States: homeUp}, blacksmith, true, 0},
		{"collector failure writes nothing", 16 * time.Minute, collect.Result{States: homeUp, Errors: map[string]string{"github": "403"}}, "home-ci", false, -1},
		{"home back after hold switches only ci", 17 * time.Minute, collect.Result{States: homeUp}, "home-ci", false, 3},
	}
	for _, s := range steps {
		vars.writes = 0
		line := step(t, c, s.at, s.result)
		ci := route(t, line, "ci")
		if ci.Label != s.label || ci.Held != s.held || route(t, line, "bot").Label != blacksmith {
			t.Fatalf("%s: got %+v", s.name, line.Routes)
		}
		if s.written < 0 {
			if vars.writes != 0 || ci.Written != nil || line.Errors["write"] == "" {
				t.Fatalf("%s: wrote %d, line %+v", s.name, vars.writes, line)
			}
			continue
		}
		if n := len(ci.Written) + len(route(t, line, "bot").Written); n != s.written || vars.writes != s.written {
			t.Fatalf("%s: written %d, %d requests", s.name, n, vars.writes)
		}
	}
	a, err := apply.LoadState(c.appliedPath)
	if err != nil || a.Routes["ci"].Label != "home-ci" || !a.Routes["ci"].Since.Equal(now.Add(17*time.Minute)) || a.Routes["bot"].Label != blacksmith {
		t.Fatalf("applied %+v %v", a, err)
	}
	if vars.values["BOT_RUNNER"] != blacksmith || vars.values["CI_RUNNER"] != "home-ci" {
		t.Fatalf("variables %v", vars.values)
	}
}

func TestSwitchModePartialWriteStartsHold(t *testing.T) {
	c := newController(t, "switch")
	c.cfg.Repositories = []string{"acme/ok", "acme/denied"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/denied/"):
			w.WriteHeader(http.StatusForbidden)
		case r.Method == http.MethodGet:
			fmt.Fprint(w, `{"value":"home-ci"}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	gh := github.New("t")
	gh.BaseURL = srv.URL
	c.writer = &apply.Writer{GH: gh}
	c.appliedPath = filepath.Join(t.TempDir(), "applied.json")
	c.state.Routes = map[string]policy.Applied{"ci": {Provider: "home", Label: "home-ci", Since: now.Add(-time.Hour)}}
	states := map[string]policy.State{"home": {OldestQueued: time.Hour}, "blacksmith": {UsedMinutes: 100}, "github": {UsedMinutes: 1000}}
	line := step(t, c, 0, collect.Result{States: states})
	if len(route(t, line, "ci").Written) != 3 || !strings.Contains(line.Errors["write ci"], "403") || line.Cancelled != nil {
		t.Fatalf("unexpected line %+v", line)
	}
	if got := c.state.Routes["ci"]; got.Provider != "blacksmith" || !got.Since.Equal(now) {
		t.Fatalf("applied %+v", got)
	}
}

func TestStuckRunsOnlyLeaveUnusableSelfHostedLabels(t *testing.T) {
	c := newController(t, "dry-run")
	queued := map[string][]collect.Run{"home": {{Repo: "your-org/your-app", ID: 7, Attempt: 1}}}
	tests := []struct {
		name   string
		chosen string
		home   policy.State
		want   int
	}{
		{"stalled home while on blacksmith", blacksmith, policy.State{Stalled: true}, 1},
		{"saturated home while on blacksmith", blacksmith, policy.State{Online: true, OldestQueued: time.Hour}, 1},
		{"disabled home while on blacksmith", blacksmith, policy.State{Online: true, Disabled: "away"}, 1},
		{"online home held on blacksmith", blacksmith, policy.State{Online: true}, 0},
		{"home chosen as fallback keeps its queue", "home-ci", policy.State{Stalled: true}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := collect.Result{Movable: queued}
			states := map[string]policy.State{"home": tt.home}
			if got := c.stuck(map[string]bool{tt.chosen: true}, states, r); len(got) != tt.want {
				t.Fatalf("got %v, want %d runs", got, tt.want)
			}
		})
	}
}
