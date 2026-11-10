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

	"github.com/morhaf-ko/zerobill/internal/apply"
	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/config"
	"github.com/morhaf-ko/zerobill/internal/discover"
	"github.com/morhaf-ko/zerobill/internal/github"
	pauses "github.com/morhaf-ko/zerobill/internal/pause"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func TestBuild(t *testing.T) {
	providers, routes := build(&config.Config{ReservePercent: config.DefaultReservePercent, Owner: "acme"}, 2000)
	if len(providers) != 1 || routes[0].Fallback.Name != "waiting" || len(routes[1].Providers) != 1 {
		t.Fatalf("GitHub only: %+v %+v", providers, routes)
	}
	providers, routes = build(&config.Config{ReservePercent: config.DefaultReservePercent, Owner: "acme", SelfHostedLabel: "home-ci", Blacksmith: true}, 3000)
	var names []string
	for _, p := range routes[0].Providers {
		names = append(names, p.Name)
	}
	if !slices.Equal(names, []string{"runner", "blacksmith", "github"}) || len(providers) != 3 ||
		routes[1].Variable != "BOT_RUNNER" || len(routes[1].Providers) != 2 || routes[1].Fallback.Name != "runner" || providers[2].Monthly != 3000 {
		t.Fatalf("full: %+v %+v", providers, routes)
	}
}

func TestRecoveryAlertRetriesFailedDeliveryAndSurvivesRestart(t *testing.T) {
	c, vars := newTestController(t, true)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/app/actions/runs/42" {
			fmt.Fprint(w, "{\"status\":\"queued\",\"run_attempt\":1}")
			return
		}
		vars.ServeHTTP(w, r)
	}))
	defer gh.Close()
	c.a.gh.BaseURL = gh.URL
	attempts := 0
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "recovery needs attention") {
			attempts++
			if attempts == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()
	c.a.cfg.Notify = hook.URL
	c.state.Cancelled = map[string]apply.Moved{"acme/app#42": {
		Run: collect.Run{Repo: "acme/app", ID: 42, Attempt: 1}, At: now,
		Stage: "needs_attention", Problem: "force cancellation did not complete",
	}}
	l := step(t, c, 0, result(policy.State{}))
	if attempts != 1 || l.Errors["notify recovery acme/app#42"] == "" || c.state.Cancelled["acme/app#42"].Alerted {
		t.Fatalf("failed delivery: %d %+v", attempts, l)
	}
	l = step(t, c, time.Minute, result(policy.State{}))
	if attempts != 2 || !c.state.Cancelled["acme/app#42"].Alerted || !strings.Contains(l.Recovery["acme/app#42"], "https://github.com/acme/app/actions/runs/42") {
		t.Fatalf("successful delivery: %d %+v", attempts, l)
	}
	restarted, err := newController(c.a, true)
	if err != nil {
		t.Fatal(err)
	}
	restarted.repos = c.repos
	step(t, restarted, 2*time.Minute, result(policy.State{}))
	if attempts != 2 {
		t.Fatalf("restart duplicated alert: %d", attempts)
	}
	preview, err := newController(c.a, false)
	if err != nil {
		t.Fatal(err)
	}
	m := preview.state.Cancelled["acme/app#42"]
	m.Alerted = false
	preview.state.Cancelled["acme/app#42"] = m
	step(t, preview, 3*time.Minute, result(policy.State{}))
	if attempts != 2 {
		t.Fatal("log-only mode sent a recovery alert")
	}
}

func TestRecoveryJourneyWritesTargetsBeforeCancelAndVerifiesRerun(t *testing.T) {
	c, vars := newTestController(t, true)
	c.repos = c.repos[:1]
	attempt, status, conclusion := 1, "queued", ""
	var posts []string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/actions/variables") {
			vars.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodPost {
			action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			posts = append(posts, action)
			if vars.values["acme/app/CI_RUNNER"] != blacksmithLabel {
				t.Error("cancelled before updating the route")
			}
			switch action {
			case "cancel":
			case "force-cancel":
				status, conclusion = "completed", "cancelled"
			case "rerun-failed-jobs":
				attempt++
				status, conclusion = "queued", ""
			default:
				t.Errorf("unexpected POST %s", r.URL.Path)
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/jobs") {
			label := "home-ci"
			if attempt == 2 {
				label = blacksmithLabel
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []any{map[string]any{
				"id": 1, "status": status, "conclusion": conclusion, "labels": []string{label}, "runner_id": 0, "steps": []any{},
			}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "conclusion": conclusion, "run_attempt": attempt})
	}))
	defer gh.Close()
	c.a.gh.BaseURL = gh.URL
	r := result(policy.State{Registered: true, OldestQueued: 20 * time.Minute})
	r.Movable = map[string][]collect.Run{"runner": {{Repo: "acme/app", ID: 1, Attempt: 1, Label: "home-ci"}}}
	l := step(t, c, 0, r)
	if len(l.Cancelled) != 1 {
		t.Fatalf("first check: %+v", l)
	}
	restart := func() {
		next, err := newController(c.a, true)
		if err != nil {
			t.Fatal(err)
		}
		next.repos = c.repos
		c = next
	}
	restart()
	if l = step(t, c, 4*time.Minute, r); len(l.Forced) != 0 {
		t.Fatal("forced before grace")
	}
	if l = step(t, c, 5*time.Minute, r); len(l.Forced) != 1 {
		t.Fatalf("force: %+v", l)
	}
	restart()
	if l = step(t, c, 6*time.Minute, r); len(l.Rerun) != 0 {
		t.Fatal("rerun not yet verified")
	}
	if l = step(t, c, 7*time.Minute, r); len(l.Rerun) != 1 || len(l.Errors) != 0 {
		t.Fatalf("verification: %+v", l)
	}
	if !slices.Equal(posts, []string{"cancel", "force-cancel", "rerun-failed-jobs"}) {
		t.Fatal(posts)
	}
}

func TestConfiguredReserveRoutesAndHolds(t *testing.T) {
	for _, reserve := range []int{0, 25, 99} {
		_, routes := build(&config.Config{Owner: "acme", SelfHostedLabel: "home-ci", Blacksmith: true, ReservePercent: reserve}, 2000)
		for _, rt := range routes {
			states := map[string]policy.State{"runner": {Registered: true}}
			for _, p := range rt.Providers {
				if p.Kind == policy.Metered {
					want := p.Monthly * float64(100-reserve) / 100
					if p.Limit() != want {
						t.Fatalf("reserve %d, provider %s limit %v, want %v", reserve, p.Name, p.Limit(), want)
					}
					states[p.Name] = policy.State{UsedMinutes: want}
				}
			}
			d := policy.Decide(rt, states)
			if d.Provider != "runner" {
				t.Fatalf("reserve %d: %+v", reserve, d)
			}
			held, ok := policy.Hold(d, policy.Applied{Provider: "blacksmith", Label: blacksmithLabel, Since: now}, rt, states, now)
			if ok || held.Provider != "runner" {
				t.Fatalf("reserve %d held exhausted provider: %+v", reserve, held)
			}
		}
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
	a := &app{cfg: &config.Config{ReservePercent: config.DefaultReservePercent, Owner: "acme", SelfHostedLabel: "home-ci", Blacksmith: true}, gh: gh, stateDir: t.TempDir()}
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
		// The second check may come a little under an interval after a late first one.
		"warm pool": {
			{0, idle, "home-ci"},
			{4*time.Minute + 49*time.Second, idle, "home-ci"},
			{10 * time.Minute, stall, "blacksmith-2vcpu-ubuntu-2404"},
			{5 * time.Hour, policy.State{}, "blacksmith-2vcpu-ubuntu-2404"},
			{5*time.Hour + 5*time.Minute, idle, "home-ci"},
		},
		// Idle sightings far apart do not make a pool warm.
		"stale sighting": {
			{0, idle, "home-ci"},
			{30 * time.Minute, idle, "home-ci"},
			{35 * time.Minute, stall, "blacksmith-2vcpu-ubuntu-2404"},
			{96 * time.Minute, policy.State{}, "home-ci"},
		},
		// A warm pool still off is tried again after six hours.
		"warm pool retried": {
			{0, idle, "home-ci"},
			{5 * time.Minute, idle, "home-ci"},
			{10 * time.Minute, stall, "blacksmith-2vcpu-ubuntu-2404"},
			{5 * time.Hour, policy.State{}, "blacksmith-2vcpu-ubuntu-2404"},
			{6*time.Hour + 15*time.Minute, policy.State{}, "home-ci"},
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

func TestHealth(t *testing.T) {
	h := &heartbeat{}
	get := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code, rec.Body.String()
	}
	h.mark(time.Now().Add(-time.Minute))
	if code, body := get("/healthz"); code != http.StatusOK || !strings.HasPrefix(body, "ok, last check 1m0s ago") {
		t.Fatalf("fresh: %d %q", code, body)
	}
	h.mark(time.Now().Add(-staleAfter - time.Minute))
	if code, body := get("/healthz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "no check for 16m0s") {
		t.Fatalf("stale: %d %q", code, body)
	}
	if code, _ := get("/"); code != http.StatusNotFound {
		t.Fatalf("other paths: %d", code)
	}
}

func TestRender(t *testing.T) {
	a := &app{cfg: &config.Config{ReservePercent: config.DefaultReservePercent, Owner: "acme", SelfHostedLabel: "home-ci", Blacksmith: true}}
	a.providers, a.routes = build(a.cfg, 2000)
	states := map[string]policy.State{"runner": {Online: true}, "blacksmith": {UsedMinutes: 1200}, "github": {UsedMinutes: 200}}
	found := discover.Result{Repos: []discover.Repo{{Name: "acme/app"}}, Fixed: []string{"acme/app .github/workflows/ci.yml:5: ubuntu-latest"}}
	var out bytes.Buffer
	if err := render(&out, time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC), a, nil, states, found, collect.Result{}); err != nil {
		t.Fatal(err)
	}
	// 6 of 30 days gone: Blacksmith's 1200 minutes project to 6000, past its 2700 on 14 Nov.
	if strings.Contains(out.String(), "\nBOT_RUNNER") {
		t.Fatalf("unused bot route shown: %s", out.String())
	}
	for _, want := range []string{"20% of the month", "6000 min, runs out 14 Nov", "1500 min", "CI_RUNNER  home-ci", "fixed runner, change to ${{ vars.CI_RUNNER }}: acme/app"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
}

func TestCapacityAlertsPersistAndReset(t *testing.T) {
	var messages []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "monthly free allowance") {
			messages = append(messages, string(body))
		}
	}))
	defer hook.Close()
	c, _ := newTestController(t, true)
	c.a.cfg.Notify, c.a.cfg.AlertRemainingPercent = hook.URL, 20
	r := result(policy.State{Registered: true})
	r.States["github"] = policy.State{UsedMinutes: 1600}
	r.States["blacksmith"] = policy.State{UsedMinutes: 2400}
	l := step(t, c, 0, r)
	if len(messages) != 2 || len(l.Alerts) != 2 || l.Errors != nil {
		t.Fatalf("initial: %q %+v", messages, l)
	}
	restarted, err := newController(c.a, true)
	if err != nil {
		t.Fatal(err)
	}
	restarted.repos = c.repos
	step(t, restarted, time.Minute, r)
	if len(messages) != 2 {
		t.Fatalf("restart duplicated alerts: %q", messages)
	}
	// Owner changes and the next UTC month must not inherit another allowance's warning.
	restarted.a.cfg.Owner = "other"
	step(t, restarted, 2*time.Minute, r)
	if len(messages) != 4 {
		t.Fatalf("owner reset: %q", messages)
	}
	step(t, restarted, 32*24*time.Hour, r)
	if len(messages) != 6 {
		t.Fatalf("month reset: %q", messages)
	}
}

func TestCapacityAlertsRetryAndReadFailures(t *testing.T) {
	sends := 0
	fail := true
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "monthly free allowance") {
			sends++
			if fail {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		}
	}))
	defer hook.Close()
	c, _ := newTestController(t, true)
	c.a.cfg.Notify, c.a.cfg.AlertRemainingPercent = hook.URL, 5
	r := result(policy.State{Registered: true})
	r.States["github"] = policy.State{UsedMinutes: 1900}
	l := step(t, c, 0, r)
	if sends != 1 || l.Errors["notify capacity github"] == "" || c.state.Alerts["github"] {
		t.Fatalf("failed send: %d %+v", sends, l)
	}
	fail = false
	l = step(t, c, time.Minute, r)
	if sends != 2 || !c.state.Alerts["github"] || l.Errors != nil {
		t.Fatalf("retry: %d %+v", sends, l)
	}
	c.state.Alerts = map[string]bool{}
	r.Errors = map[string]string{"blacksmith": "unavailable"}
	l = step(t, c, 2*time.Minute, r)
	if sends != 2 || len(l.Alerts) != 0 {
		t.Fatalf("read failure sent warning: %d %+v", sends, l)
	}
	c.apply = false
	r.Errors = nil
	l = step(t, c, 3*time.Minute, r)
	if sends != 2 || len(l.Alerts) != 1 {
		t.Fatalf("preview sent capacity warning: %d %+v", sends, l)
	}
	c.a.cfg.AlertRemainingPercent = 0
	l = step(t, c, 4*time.Minute, r)
	if len(l.Alerts) != 0 {
		t.Fatalf("disabled warnings: %+v", l)
	}
}

func TestHostedOnlyExhaustionWaits(t *testing.T) {
	c, vars := newTestController(t, true)
	c.a.cfg.SelfHostedLabel = ""
	c.a.providers, c.a.routes = build(c.a.cfg, 2000)
	r := collect.Result{States: map[string]policy.State{"github": {UsedMinutes: 1800}, "blacksmith": {UsedMinutes: 2700}}}
	l := step(t, c, 0, r)
	if l.Errors != nil {
		t.Fatal(l.Errors)
	}
	for key, value := range vars.values {
		if value != config.WaitingLabel {
			t.Fatalf("%s selected paid capacity %s", key, value)
		}
	}
}

func TestConfiguredQueueTimeoutLatchesAndRecovers(t *testing.T) {
	c, _ := newTestController(t, false)
	c.a.cfg.QueueTimeout = 2 * time.Minute
	c.a.cfg.CheckInterval = time.Minute
	c.a.providers, c.a.routes = build(c.a.cfg, 2000)
	stall := policy.State{OldestQueued: 3 * time.Minute}
	if l := step(t, c, 0, result(stall)); l.Routes[0].Provider != "blacksmith" {
		t.Fatalf("stall not detected: %+v", l)
	}
	if l := step(t, c, time.Minute, result(policy.State{})); l.Routes[0].Provider != "blacksmith" {
		t.Fatalf("stall not remembered: %+v", l)
	}
	if l := step(t, c, 2*time.Minute, result(policy.State{Online: true})); l.Routes[0].Provider != "runner" || l.Routes[0].Held {
		t.Fatalf("recovery held: %+v", l)
	}
}

func TestHealthUsesConfiguredInterval(t *testing.T) {
	h := &heartbeat{staleAfter: 3 * time.Hour}
	h.mark(time.Now().Add(-time.Hour))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("hourly loop considered stale: %d", rec.Code)
	}
	h.mark(time.Now().Add(-4 * time.Hour))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("stalled loop considered healthy: %d", rec.Code)
	}
}
