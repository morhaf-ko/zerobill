package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/morhaf-ko/zerobill/internal/policy"
	"gopkg.in/yaml.v3"
)

func TestLoadExample(t *testing.T) {
	c, err := Load("../../examples/zerobill.yaml")
	if err != nil {
		t.Fatal(err)
	}
	providers, err := c.PolicyProviders()
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != DryRun || c.Hold() != DefaultMinHold {
		t.Fatalf("unexpected defaults: %s %s", c.Mode, c.Hold())
	}
	if providers[0].MaxQueueWait != 10*time.Minute || providers[1].Multiplier != 1 {
		t.Fatalf("unexpected providers: %+v", providers)
	}
	states, err := LoadObservation("../../examples/observed.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if states["blacksmith"].UsedMinutes != 1240 || states["home"].Online {
		t.Fatalf("unexpected states: %+v", states)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := map[string]string{
		"live mode":        "mode: live\nfallback: a\nproviders: [{name: a, label: x, kind: self-hosted}]",
		"bad min_hold":     "min_hold: soon\nfallback: a\nproviders: [{name: a, label: x, kind: self-hosted}]",
		"switch no repos":  "mode: switch\nvariables: [CI_RUNNER]\nfallback: a\nproviders: [{name: a, label: x, kind: self-hosted}]",
		"switch bad repo":  "mode: switch\nrepositories: [app]\nvariables: [CI_RUNNER]\nfallback: a\nproviders: [{name: a, label: x, kind: self-hosted}]",
		"unknown fallback": "fallback: b\nproviders: [{name: a, label: x, kind: self-hosted}]",
		"metered budget":   "fallback: a\nproviders: [{name: a, label: x, kind: metered}]",
		"duplicate":        "fallback: a\nproviders: [{name: a, label: x, kind: self-hosted}, {name: a, label: y, kind: self-hosted}]",
		"reserve too big":  "fallback: a\nproviders: [{name: a, label: x, kind: metered, monthly_minutes: 100, reserve_minutes: 100}]",
		"on_demand wait":   "fallback: a\nproviders: [{name: a, label: x, kind: self-hosted, on_demand: true}]",
		"bad retry_after":  "fallback: a\nproviders: [{name: a, label: x, kind: self-hosted, max_queue_wait: 1m, on_demand: true, retry_after: later}]",
		"route provider":   "fallback: a\nproviders: [{name: a, label: x, kind: self-hosted}]\nroutes: [{name: r, variables: [V], providers: [b]}]",
		"route fallback":   "providers: [{name: a, label: x, kind: self-hosted}]\nroutes: [{name: r, variables: [V], providers: [a]}]",
		"route balance":    "fallback: a\nproviders: [{name: a, label: x, kind: self-hosted}]\nroutes: [{name: r, variables: [V], providers: [a], balance: random}]",
		"route name":       "fallback: a\nproviders: [{name: a, label: x, kind: self-hosted}]\nroutes: [{name: r, providers: [a]}, {name: r, providers: [a]}]",
		"shared variable":  "fallback: a\nproviders: [{name: a, label: x, kind: self-hosted}]\nroutes: [{name: r, variables: [V], providers: [a]}, {name: s, variables: [V], providers: [a]}]",
		"top variables":    "fallback: a\nvariables: [V]\nproviders: [{name: a, label: x, kind: self-hosted}]\nroutes: [{name: r, variables: [W], providers: [a]}]",
		"switch no vars":   "mode: switch\nrepositories: [o/app]\nfallback: a\nproviders: [{name: a, label: x, kind: self-hosted}]\nroutes: [{name: r, providers: [a]}]",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("expected a path-qualified error, got %v", err)
			}
		})
	}
}

func TestRoutes(t *testing.T) {
	c, err := Load("../../examples/zerobill.yaml")
	if err != nil {
		t.Fatal(err)
	}
	providers, err := c.PolicyProviders()
	if err != nil {
		t.Fatal(err)
	}
	if !providers[0].OnDemand || providers[0].RetryAfter != time.Hour {
		t.Fatalf("unexpected home provider %+v", providers[0])
	}
	routes := c.PolicyRoutes(providers)
	if len(routes) != 2 || routes[1].Name != "bot" || len(routes[1].Providers) != 2 || routes[1].Fallback.Name != "home" || routes[1].Balance != policy.Pace {
		t.Fatalf("unexpected routes %+v", routes)
	}

	path := filepath.Join(t.TempDir(), "c.yaml")
	single := "mode: switch\nrepositories: [o/app]\nvariables: [CI_RUNNER]\nfallback: a\nproviders: [{name: a, label: x, kind: self-hosted}, {name: b, label: y, kind: metered, monthly_minutes: 10}]"
	if err := os.WriteFile(path, []byte(single), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	providers, err = c.PolicyProviders()
	if err != nil {
		t.Fatal(err)
	}
	routes = c.PolicyRoutes(providers)
	if len(routes) != 1 || routes[0].Name != DefaultRoute || len(routes[0].Providers) != 2 || routes[0].Variables[0] != "CI_RUNNER" || routes[0].Balance != policy.Order {
		t.Fatalf("top-level variables should form the default route: %+v", routes)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	c, err := Load("../../examples/zerobill.yaml")
	if err != nil {
		t.Fatal(err)
	}
	providers, err := c.PolicyProviders()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]policy.State{
		"home":       {Online: true, OldestQueued: 3 * time.Minute},
		"blacksmith": {Online: true, UsedMinutes: 1240},
		"github":     {Online: true, UsedMinutes: 0},
	}
	raw, err := yaml.Marshal(Snapshot(providers, want))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "observed.yaml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	if got["home"] != want["home"] || got["blacksmith"].UsedMinutes != 1240 || !strings.Contains(string(raw), "used_minutes: 0") {
		t.Fatalf("round trip lost state:\n%s", raw)
	}
}
