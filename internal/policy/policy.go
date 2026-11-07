// Package policy picks the runner label that keeps CI inside free capacity.
package policy

import (
	"fmt"
	"time"
)

// Kind distinguishes hardware you own from providers that meter minutes.
type Kind string

const (
	SelfHosted Kind = "self-hosted"
	Metered    Kind = "metered"
)

// Provider is one place jobs can run, in priority order.
type Provider struct {
	Name  string
	Label string
	Kind  Kind
	// MonthlyMinutes and ReserveMinutes apply to metered providers. The reserve is
	// kept back so a switch never lands on a provider that runs dry mid-job.
	MonthlyMinutes float64
	ReserveMinutes float64
	// Multiplier converts wall-clock job minutes into billed minutes, for example 2
	// for a 4-vCPU Blacksmith runner.
	Multiplier float64
	// MaxQueueWait marks a self-hosted provider saturated when its oldest queued job
	// has waited longer. Zero disables the check.
	MaxQueueWait time.Duration
}

// State is what collectors observed about one provider.
type State struct {
	Online       bool
	UsedMinutes  float64
	OldestQueued time.Duration
}

// Decision is the label to write and why.
type Decision struct {
	Provider string
	Label    string
	Reason   string
	Fallback bool
}

// Remaining returns billed minutes left before the reserve.
func (p Provider) Remaining(s State) float64 {
	return p.MonthlyMinutes - s.UsedMinutes - p.ReserveMinutes
}

// Usable reports whether p can take jobs in state s and why.
func Usable(p Provider, s State) (bool, string, error) {
	switch p.Kind {
	case SelfHosted:
		if !s.Online {
			return false, "offline", nil
		}
		if p.MaxQueueWait > 0 && s.OldestQueued > p.MaxQueueWait {
			return false, fmt.Sprintf("queue waited %s", s.OldestQueued.Round(time.Second)), nil
		}
		return true, "online", nil
	case Metered:
		left := p.Remaining(s)
		if left <= 0 {
			return false, "free minutes at reserve", nil
		}
		return true, fmt.Sprintf("%.0f free minutes above reserve", left), nil
	default:
		return false, "", fmt.Errorf("provider %s: unknown kind %q", p.Name, p.Kind)
	}
}

// Decide returns the first usable provider. When none is usable it returns the
// fallback provider so jobs queue instead of spending money.
func Decide(providers []Provider, states map[string]State, fallback string) (Decision, error) {
	var skipped []string
	for _, p := range providers {
		s, ok := states[p.Name]
		if !ok {
			skipped = append(skipped, p.Name+": no observation")
			continue
		}
		ok, why, err := Usable(p, s)
		if err != nil {
			return Decision{}, err
		}
		if !ok {
			skipped = append(skipped, p.Name+": "+why)
			continue
		}
		return Decision{Provider: p.Name, Label: p.Label, Reason: reason(why, skipped)}, nil
	}
	for _, p := range providers {
		if p.Name == fallback {
			return Decision{Provider: p.Name, Label: p.Label, Reason: reason("no free capacity, jobs queue", skipped), Fallback: true}, nil
		}
	}
	return Decision{}, fmt.Errorf("fallback provider %q is not configured", fallback)
}

func reason(chosen string, skipped []string) string {
	if len(skipped) == 0 {
		return chosen
	}
	r := chosen + " (skipped "
	for i, s := range skipped {
		if i > 0 {
			r += "; "
		}
		r += s
	}
	return r + ")"
}

// Applied is the label last written to the repository variables.
type Applied struct {
	Provider string    `json:"provider"`
	Label    string    `json:"label"`
	Since    time.Time `json:"since"`
}

// Hold keeps the applied label when d would move back to a higher-priority
// provider before minHold has passed. A move away from an applied provider that
// is no longer usable is never held. It returns the decision to apply and
// whether it was held.
func Hold(d Decision, applied Applied, providers []Provider, states map[string]State, now time.Time, minHold time.Duration) (Decision, bool) {
	if applied.Label == "" || applied.Label == d.Label || now.Sub(applied.Since) >= minHold {
		return d, false
	}
	for _, p := range providers {
		if p.Name != applied.Provider || p.Label != applied.Label {
			continue
		}
		s, ok := states[p.Name]
		if !ok {
			return d, false
		}
		if usable, why, err := Usable(p, s); err == nil && usable {
			wait := (minHold - now.Sub(applied.Since)).Round(time.Second)
			return Decision{
				Provider: p.Name, Label: p.Label,
				Reason: fmt.Sprintf("held %s for %s more; %s preferred: %s", why, wait, d.Provider, d.Reason),
			}, true
		}
		return d, false
	}
	return d, false
}
