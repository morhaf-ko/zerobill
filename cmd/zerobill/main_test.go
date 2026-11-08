package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/config"
	"github.com/morhaf-ko/zerobill/internal/discover"
	"github.com/morhaf-ko/zerobill/internal/github"
	pauses "github.com/morhaf-ko/zerobill/internal/pause"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func TestBuild(t *testing.T) {
	providers, routes := build(&config.Config{Owner: "acme"}, 2000)
	if len(providers) != 1 || routes[0].Fallback.Name != "github" || len(routes[1].Providers) != 1 {
		t.Fatalf("GitHub only: %+v %+v", providers, routes)
	}
	providers, routes = build(&config.Config{Owner: "acme", Runner: "home-ci", Blacksmith: true}, 3000)
	var names []string
	for _, p := range routes[0].Providers {
		names = append(names, p.Name)
	}
	if !slices.Equal(names, []string{"runner", "blacksmith", "github"}) || len(providers) != 3 ||
		routes[1].Variable != "BOT_RUNNER" || len(routes[1].Providers) != 2 || routes[1].Fallback.Name != "runner" || providers[2].Monthly != 3000 {
		t.Fatalf("full: %+v %+v", providers, routes)
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "72h": 72 * time.Hour, "90m": 90 * time.Minute} {
		if got, err := parseDuration(in); err != nil || got != want {
			t.Errorf("%s: got %v %v", in, got, err)
		}
	}
	for _, in := range []string{"0d", "-1h", "soon"} {
		if _, err := parseDuration(in); err == nil {
			t.Errorf("%s: want an error", in)
		}
	}
}

// variables fakes the repository variables API.
type variables struct{ values map[string]string }

func (v *variables) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/repos/")
	key = strings.Replace(key, "/actions/variables", "", 1)
	if r.Method == http.MethodGet {
		val, ok := v.values[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"value":%q}`, val)
		return
	}
	var in struct{ Name, Value string }
	_ = json.NewDecoder(r.Body).Decode(&in)
	if !strings.HasSuffix(key, "/"+in.Name) {
		key += "/" + in.Name
	}
	v.values[key] = in.Value
	w.WriteHeader(http.StatusNoContent)
}

func newTestController(t *testing.T, write bool) (*controller, *variables) {
	t.Helper()
	vars := &variables{values: map[string]string{}}
	srv := httptest.NewServer(vars)
	t.Cleanup(srv.Close)
	gh := github.New("t")
	gh.BaseURL = srv.URL
	a := &app{cfg: &config.Config{Owner: "acme", Runner: "home-ci", Blacksmith: true}, gh: gh, stateDir: t.TempDir()}
	a.providers, a.routes = build(a.cfg, 2000)
	c, err := newController(a, write)
	if err != nil {
		t.Fatal(err)
	}
	c.repos = []discover.Repo{{Name: "acme/app", Vars: []string{"CI_RUNNER"}}, {Name: "acme/bots", Vars: []string{"BOT_RUNNER", "CI_RUNNER"}}}
	return c, vars
}

func step(t *testing.T, c *controller, at time.Duration, r collect.Result) line {
	t.Helper()
	var buf bytes.Buffer
	if err := c.step(&buf, now.Add(at), r); err != nil {
		t.Fatal(err)
	}
	var l line
	if err := json.Unmarshal(buf.Bytes(), &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func result(runner policy.State) collect.Result {
	return collect.Result{States: map[string]policy.State{"runner": runner, "blacksmith": {UsedMinutes: 900}, "github": {UsedMinutes: 700}}}
}

func TestRunWritesOnlyVariablesEachRepositoryReads(t *testing.T) {
	c, vars := newTestController(t, true)
	l := step(t, c, 0, result(policy.State{Online: true, Registered: true}))
	if l.Errors != nil || l.Routes[0].Label != "home-ci" || l.Routes[1].Label != "blacksmith-2vcpu-ubuntu-2404" {
		t.Fatalf("line %+v", l)
	}
	want := map[string]string{"acme/app/CI_RUNNER": "home-ci", "acme/bots/CI_RUNNER": "home-ci", "acme/bots/BOT_RUNNER": "blacksmith-2vcpu-ubuntu-2404"}
	if fmt.Sprint(vars.values) != fmt.Sprint(want) {
		t.Fatalf("variables %v", vars.values)
	}

	// A failed collector writes nothing.
	r := result(policy.State{Registered: true})
	r.Errors = map[string]string{"github": "403"}
	l = step(t, c, 20*time.Minute, r)
	if l.Errors["write"] == "" || vars.values["acme/app/CI_RUNNER"] != "home-ci" {
		t.Fatalf("wrote despite a collector error: %+v %v", l, vars.values)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	c, vars := newTestController(t, false)
	l := step(t, c, 0, result(policy.State{Registered: true}))
	if l.Apply || l.Routes[0].Written != nil || len(vars.values) != 0 || l.Routes[0].Provider != "blacksmith" {
		t.Fatalf("line %+v values %v", l, vars.values)
	}
	if _, err := os.Stat(c.a.appliedPath()); !os.IsNotExist(err) {
		t.Fatalf("dry run saved state: %v", err)
	}
}

func TestQueueStallRecovery(t *testing.T) {
	type at struct {
		at    time.Duration
		s     policy.State
		label string
	}
	idle := policy.State{Online: true, Registered: true, Idle: true}
	stall := policy.State{OldestQueued: 11 * time.Minute}
	for name, steps := range map[string][]at{
		// Warm runners stay idle across checks, so the pool stays down until one is back.
		"warm pool": {
			{0, idle, "home-ci"},
			{5 * time.Minute, idle, "home-ci"},
			{10 * time.Minute, stall, "blacksmith-2vcpu-ubuntu-2404"},
			{8 * time.Hour, policy.State{}, "blacksmith-2vcpu-ubuntu-2404"},
			{8*time.Hour + 5*time.Minute, idle, "home-ci"},
		},
		// A per-job runner idle on a single check does not make the pool warm,
		// and a job stuck in the queue does not postpone the hourly retry.
		"per-job pool": {
			{0, idle, "home-ci"},
			{5 * time.Minute, stall, "blacksmith-2vcpu-ubuntu-2404"},
			{30 * time.Minute, stall, "blacksmith-2vcpu-ubuntu-2404"},
			{66 * time.Minute, policy.State{}, "home-ci"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := newTestController(t, false)
			for _, s := range steps {
				if got := step(t, c, s.at, result(s.s)).Routes[0].Label; got != s.label {
					t.Fatalf("at %s: got %s", s.at, got)
				}
			}
		})
	}
}

func TestPauseTakesTheRunnerOut(t *testing.T) {
	c, _ := newTestController(t, false)
	if err := (pauses.Set{"runner": now.Add(time.Hour)}).Save(c.a.pausePath()); err != nil {
		t.Fatal(err)
	}
	l := step(t, c, 0, result(policy.State{Online: true, Registered: true}))
	if l.Routes[0].Provider != "blacksmith" || !strings.Contains(l.Paused["runner"], "paused until") {
		t.Fatalf("line %+v", l)
	}
}

func TestNotifiesOnlyWhenTheChoiceChanges(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, string(b))
	}))
	defer srv.Close()
	c, _ := newTestController(t, false)
	c.a.cfg.Notify = srv.URL + "/topic"
	step(t, c, 0, result(policy.State{Online: true, Registered: true}))
	step(t, c, 5*time.Minute, result(policy.State{Online: true, Registered: true}))
	step(t, c, 20*time.Minute, result(policy.State{Registered: true}))
	if len(got) != 2 || !strings.HasPrefix(got[0], "zerobill: CI_RUNNER → home-ci (online); BOT_RUNNER → blacksmith") ||
		!strings.HasPrefix(got[1], "zerobill: CI_RUNNER → blacksmith-2vcpu-ubuntu-2404") || !strings.HasSuffix(got[1], "[log only]") {
		t.Fatalf("messages %q", got)
	}
}

func TestApplyNotifiesOnlyWhatItWrote(t *testing.T) {
	var got []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, string(b))
	}))
	defer hook.Close()
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer denied.Close()

	c, _ := newTestController(t, true)
	c.a.cfg.Notify = hook.URL + "/topic"
	c.writer.GH.BaseURL = denied.URL
	l := step(t, c, 0, result(policy.State{Online: true, Registered: true}))
	if len(got) != 0 || l.Errors == nil {
		t.Fatalf("notified %q although every write failed: %+v", got, l.Errors)
	}

	c, _ = newTestController(t, true)
	c.a.cfg.Notify = hook.URL + "/topic"
	step(t, c, 0, result(policy.State{Online: true, Registered: true}))
	if len(got) != 1 || strings.HasSuffix(got[0], "[log only]") {
		t.Fatalf("messages %q", got)
	}

	c.a.cfg.Notify = denied.URL + "/secret-hook"
	l = step(t, c, 20*time.Minute, result(policy.State{Registered: true}))
	if l.Errors["notify"] == "" || strings.Contains(l.Errors["notify"], "secret-hook") {
		t.Fatalf("notify error %q", l.Errors["notify"])
	}
}

func TestRender(t *testing.T) {
	a := &app{cfg: &config.Config{Owner: "acme", Runner: "home-ci", Blacksmith: true}}
	a.providers, a.routes = build(a.cfg, 2000)
	states := map[string]policy.State{"runner": {Online: true}, "blacksmith": {UsedMinutes: 1200}, "github": {UsedMinutes: 200}}
	found := discover.Result{Repos: []discover.Repo{{Name: "acme/app"}}, Fixed: []string{"acme/app .github/workflows/ci.yml:5: ubuntu-latest"}}
	var out bytes.Buffer
	if err := render(&out, time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC), a, nil, states, found, nil); err != nil {
		t.Fatal(err)
	}
	// 6 of 30 days gone: Blacksmith's 1200 minutes project to 6000, past its 2700 on 14 Nov.
	for _, want := range []string{"20% of the month", "6000 min, runs out 14 Nov", "1500 min", "CI_RUNNER   home-ci", "fixed runner, change to ${{ vars.CI_RUNNER }}: acme/app"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
}
