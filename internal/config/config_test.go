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
		"unknown fallback": "fallback: b\nproviders: [{name: a, label: x, kind: self-hosted}]",
		"metered budget":   "fallback: a\nproviders: [{name: a, label: x, kind: metered}]",
		"duplicate":        "fallback: a\nproviders: [{name: a, label: x, kind: self-hosted}, {name: a, label: y, kind: self-hosted}]",
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
