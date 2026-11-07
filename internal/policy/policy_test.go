package policy

import (
	"strings"
	"testing"
	"time"
)

var fleet = []Provider{
	{Name: "forge", Label: "morhaf-ci", Kind: SelfHosted, MaxQueueWait: 10 * time.Minute},
	{Name: "blacksmith", Label: "blacksmith-2vcpu-ubuntu-2404", Kind: Metered, MonthlyMinutes: 3000, ReserveMinutes: 300, Multiplier: 1},
	{Name: "github", Label: "ubuntu-24.04", Kind: Metered, MonthlyMinutes: 2000, ReserveMinutes: 200, Multiplier: 1},
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name     string
		states   map[string]State
		want     string
		fallback bool
		reason   string
	}{
		{
			name:   "self-hosted wins when online",
			states: map[string]State{"forge": {Online: true}, "blacksmith": {}, "github": {}},
			want:   "forge",
		},
		{
			name:   "offline self-hosted spills to first metered provider",
			states: map[string]State{"forge": {}, "blacksmith": {UsedMinutes: 100}, "github": {}},
			want:   "blacksmith",
			reason: "forge: offline",
		},
		{
			name:   "saturated self-hosted spills over",
			states: map[string]State{"forge": {Online: true, OldestQueued: 11 * time.Minute}, "blacksmith": {}, "github": {}},
			want:   "blacksmith",
			reason: "queue waited 11m0s",
		},
		{
			name:   "reserve is never spent",
			states: map[string]State{"forge": {}, "blacksmith": {UsedMinutes: 2700}, "github": {UsedMinutes: 500}},
			want:   "github",
			reason: "blacksmith: free minutes at reserve",
		},
		{
			name:     "no free capacity queues on the fallback",
			states:   map[string]State{"forge": {}, "blacksmith": {UsedMinutes: 3000}, "github": {UsedMinutes: 1900}},
			want:     "forge",
			fallback: true,
		},
		{
			name:   "missing observation is skipped",
			states: map[string]State{"blacksmith": {}},
			want:   "blacksmith",
			reason: "forge: no observation",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Decide(fleet, tt.states, "forge")
			if err != nil {
				t.Fatal(err)
			}
			if got.Provider != tt.want || got.Fallback != tt.fallback {
				t.Fatalf("got %+v, want provider %s fallback %v", got, tt.want, tt.fallback)
			}
			if !strings.Contains(got.Reason, tt.reason) {
				t.Fatalf("reason %q does not mention %q", got.Reason, tt.reason)
			}
		})
	}
}

func TestDecideRejectsUnknownFallback(t *testing.T) {
	if _, err := Decide(fleet, map[string]State{}, "pi"); err == nil {
		t.Fatal("expected an error for an unconfigured fallback")
	}
}

func TestHold(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	onBlacksmith := Applied{Provider: "blacksmith", Label: "blacksmith-2vcpu-ubuntu-2404", Since: now.Add(-5 * time.Minute)}
	onForge := Applied{Provider: "forge", Label: "morhaf-ci", Since: now.Add(-1 * time.Minute)}
	tests := []struct {
		name    string
		applied Applied
		states  map[string]State
		want    string
		held    bool
	}{
		{"nothing applied yet", Applied{}, map[string]State{"forge": {Online: true}, "blacksmith": {}}, "forge", false},
		{"recovery inside hold keeps metered label", onBlacksmith, map[string]State{"forge": {Online: true}, "blacksmith": {}}, "blacksmith", true},
		{"recovery after hold switches back", Applied{Provider: "blacksmith", Label: "blacksmith-2vcpu-ubuntu-2404", Since: now.Add(-15 * time.Minute)}, map[string]State{"forge": {Online: true}, "blacksmith": {}}, "forge", false},
		{"applied provider at reserve switches at once", onBlacksmith, map[string]State{"forge": {Online: true}, "blacksmith": {UsedMinutes: 2800}}, "forge", false},
		{"offline self-hosted switches at once", onForge, map[string]State{"forge": {}, "blacksmith": {}}, "blacksmith", false},
		{"unobserved applied provider switches", onBlacksmith, map[string]State{"forge": {Online: true}}, "forge", false},
		{"relabelled provider switches", Applied{Provider: "blacksmith", Label: "blacksmith-4vcpu-ubuntu-2404", Since: now}, map[string]State{"forge": {Online: true}, "blacksmith": {}}, "forge", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := Decide(fleet, tt.states, "forge")
			if err != nil {
				t.Fatal(err)
			}
			got, held := Hold(d, tt.applied, fleet, tt.states, now, 15*time.Minute)
			if got.Provider != tt.want || held != tt.held {
				t.Fatalf("got %s held=%v (%s), want %s held=%v", got.Provider, held, got.Reason, tt.want, tt.held)
			}
		})
	}
}
