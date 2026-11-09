package policy

import (
	"strings"
	"testing"
	"time"
)

var (
	runner     = Provider{Name: "runner", Label: "home-ci", Kind: SelfHosted}
	blacksmith = Provider{Name: "blacksmith", Label: "blacksmith-2vcpu-ubuntu-2404", Kind: Metered, Monthly: 3000, ReservePercent: 10}
	github     = Provider{Name: "github", Label: "ubuntu-latest", Kind: Metered, Monthly: 2000, ReservePercent: 10}
	ci         = Route{Variable: "CI_RUNNER", Providers: []Provider{runner, blacksmith, github}, Fallback: runner}
)

func TestUsable(t *testing.T) {
	tests := []struct {
		name   string
		p      Provider
		s      State
		usable bool
		why    string
	}{
		{"online runner", runner, State{Online: true, Registered: true}, true, "online"},
		{"registered but offline", runner, State{Registered: true}, false, "offline"},
		{"no runner registered, per-job pool", runner, State{}, true, "runners per job"},
		{"queue stall", runner, State{Online: true, Registered: true, OldestQueued: 11 * time.Minute}, false, "a job waited 11m0s"},
		{"stall latch", runner, State{Stalled: true}, false, "stalled"},
		{"paused", runner, State{Online: true, Paused: "paused"}, false, "paused"},
		{"free minutes left", github, State{UsedMinutes: 1000}, true, "800 free minutes left"},
		{"at the 10% reserve", github, State{UsedMinutes: 1800}, false, "reserve"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, why := Usable(tt.p, tt.s)
			if ok != tt.usable || !strings.Contains(why, tt.why) {
				t.Fatalf("got %v %q, want %v %q", ok, why, tt.usable, tt.why)
			}
		})
	}
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name   string
		states map[string]State
		want   string
		reason string
	}{
		{"runner first", map[string]State{"runner": {Online: true}, "blacksmith": {}, "github": {}}, "runner", "online"},
		{"least used free tier", map[string]State{"runner": {Registered: true}, "blacksmith": {UsedMinutes: 900}, "github": {UsedMinutes: 700}}, "blacksmith", "blacksmith 33%, github 39%"},
		{"tier at reserve is skipped", map[string]State{"runner": {Registered: true}, "blacksmith": {UsedMinutes: 2700}, "github": {UsedMinutes: 100}}, "github", "1700 free minutes left"},
		{"nothing free waits on the runner", map[string]State{"runner": {Registered: true}, "blacksmith": {UsedMinutes: 2700}, "github": {UsedMinutes: 1800}}, "runner", "jobs wait for your runner"},
		{"unobserved provider is skipped", map[string]State{"github": {}}, "github", "runner: not observed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide(ci, tt.states)
			if got.Provider != tt.want || !strings.Contains(got.Reason, tt.reason) {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestHold(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	onBlacksmith := Applied{Provider: "blacksmith", Label: blacksmith.Label, Since: now.Add(-5 * time.Minute)}
	back := map[string]State{"runner": {Online: true}, "blacksmith": {}, "github": {}}
	tests := []struct {
		name    string
		applied Applied
		states  map[string]State
		want    string
		held    bool
	}{
		{"nothing applied", Applied{}, back, "runner", false},
		{"runner back inside the hold", onBlacksmith, back, "runner", false},
		{"runner back after the hold", Applied{Provider: "blacksmith", Label: blacksmith.Label, Since: now.Add(-MinHold)}, back, "runner", false},
		{"applied tier at reserve moves at once", onBlacksmith, map[string]State{"runner": {Online: true}, "blacksmith": {UsedMinutes: 2800}}, "runner", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, held := Hold(Decide(ci, tt.states), tt.applied, ci, tt.states, now)
			if got.Provider != tt.want || held != tt.held {
				t.Fatalf("got %s held=%v (%s)", got.Provider, held, got.Reason)
			}
		})
	}
}

func TestConfiguredQueueTimeout(t *testing.T) {
	p := runner
	p.QueueTimeout = 2 * time.Minute
	for _, tt := range []struct {
		wait   time.Duration
		usable bool
	}{
		{time.Minute, true}, {2 * time.Minute, true}, {2*time.Minute + time.Second, false},
	} {
		got, _ := Usable(p, State{Online: true, OldestQueued: tt.wait})
		if got != tt.usable {
			t.Fatalf("wait %s usable %v", tt.wait, got)
		}
	}
}

func TestHostedHoldAndNoCapacity(t *testing.T) {
	rt := Route{Variable: "CI_RUNNER", Providers: []Provider{blacksmith, github}, Fallback: Provider{Name: "waiting", Label: "zerobill-wait"}}
	now := time.Now()
	applied := Applied{Provider: "github", Label: github.Label, Since: now}
	states := map[string]State{"blacksmith": {}, "github": {UsedMinutes: 1000}}
	d, held := Hold(Decide(rt, states), applied, rt, states, now)
	if !held || d.Provider != "github" {
		t.Fatalf("hosted churn: %+v held %v", d, held)
	}
	states["github"] = State{UsedMinutes: 1800}
	d, held = Hold(Decide(rt, states), applied, rt, states, now)
	if held || d.Provider != "blacksmith" {
		t.Fatalf("exhausted hold: %+v held %v", d, held)
	}
	states["blacksmith"] = State{UsedMinutes: 2700}
	d, held = Hold(Decide(rt, states), applied, rt, states, now)
	if held || d.Label != "zerobill-wait" {
		t.Fatalf("paid fallback: %+v held %v", d, held)
	}
}
