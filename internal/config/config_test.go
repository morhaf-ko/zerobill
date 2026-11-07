package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
