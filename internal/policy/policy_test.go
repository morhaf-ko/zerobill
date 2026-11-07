package policy

import (
	"strings"
	"testing"
	"time"
)

var fleet = []Provider{
	{Name: "forge", Label: "morhaf-ci", Kind: SelfHosted, MaxQueueWait: 10 * time.Minute},
	{Name: "blacksmith", Label: "blacksmith-2vcpu-ubuntu-2404", Kind: Metered, MonthlyMinutes: 3000, ReserveMinutes: 300},
	{Name: "github", Label: "ubuntu-24.04", Kind: Metered, MonthlyMinutes: 2000, ReserveMinutes: 200},
}

var ordered = Route{Name: "ci", Providers: fleet, Fallback: fleet[0], Balance: Order}

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
			got, err := Decide(ordered, tt.states)
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

func TestDecidePace(t *testing.T) {
	onDemand := fleet[0]
	onDemand.OnDemand = true
	paced := Route{Name: "ci", Providers: []Provider{onDemand, fleet[1], fleet[2]}, Fallback: onDemand, Balance: Pace}
	tests := []struct {
		name   string
		states map[string]State
		want   string
		reason string
	}{
		{"on-demand provider without runners is used", map[string]State{"forge": {}, "blacksmith": {}, "github": {}}, "forge", "on demand"},
		{"stalled on-demand provider is skipped", map[string]State{"forge": {Stalled: true}, "blacksmith": {UsedMinutes: 900}, "github": {UsedMinutes: 700}}, "blacksmith", "blacksmith 33%, github 39%"},
		{"disabled provider is skipped", map[string]State{"forge": {Online: true, Disabled: "away"}, "blacksmith": {UsedMinutes: 1500}, "github": {UsedMinutes: 300}}, "github", "forge: away"},
		{"provider at reserve is not paced", map[string]State{"forge": {Stalled: true}, "blacksmith": {UsedMinutes: 100}, "github": {UsedMinutes: 1800}}, "blacksmith", "blacksmith 4%"},
		{"unobserved metered provider is not paced", map[string]State{"forge": {Stalled: true}, "github": {UsedMinutes: 1700}}, "github", "github 94%"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Decide(paced, tt.states)
			if err != nil {
				t.Fatal(err)
			}
			if got.Provider != tt.want || !strings.Contains(got.Reason, tt.reason) {
				t.Fatalf("got %s (%s), want %s mentioning %q", got.Provider, got.Reason, tt.want, tt.reason)
			}
		})
	}
	bot := Route{Name: "bot", Providers: fleet[1:], Fallback: fleet[0], Balance: Pace}
	got, err := Decide(bot, map[string]State{"forge": {}, "blacksmith": {UsedMinutes: 2700}, "github": {UsedMinutes: 1800}})
	if err != nil || got.Provider != "forge" || !got.Fallback {
		t.Fatalf("bot route without free minutes should queue on forge: %+v %v", got, err)
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
			d, err := Decide(ordered, tt.states)
			if err != nil {
				t.Fatal(err)
			}
			got, held := Hold(d, tt.applied, ordered, tt.states, now, 15*time.Minute)
			if got.Provider != tt.want || held != tt.held {
				t.Fatalf("got %s held=%v (%s), want %s held=%v", got.Provider, held, got.Reason, tt.want, tt.held)
			}
		})
	}
}
