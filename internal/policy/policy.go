// Package policy picks the runner label that keeps CI inside free capacity.
package policy

import (
	"fmt"
	"strings"
	"time"
)

// Conventions every user shares.
const (
	// MaxQueueWait marks a self-hosted runner down when a job has waited longer.
	MaxQueueWait = 10 * time.Minute
	// RetryAfter is when a stalled pool that never shows idle runners is tried
	// again, since it only registers a runner once a job arrives.
	RetryAfter = time.Hour
	// WarmRetryAfter is the same for a pool that keeps idle runners, whose
	// return is normally seen first; it bounds a mistaken warm mark.
	WarmRetryAfter = 6 * time.Hour
	// MinHold is how long an applied label stays while it is still usable.
	MinHold = 15 * time.Minute
)

// Kind distinguishes hardware you own from providers that meter minutes.
type Kind string

const (
	SelfHosted Kind = "self-hosted"
	Metered    Kind = "metered"
)

// Provider is one place jobs can run.
type Provider struct {
	Name  string
	Label string
	Kind  Kind
	// Monthly is a metered provider's free minutes a month.
	Monthly        float64
	ReservePercent int
	QueueTimeout   time.Duration
}

// Limit is the metered minutes usable before the reserve.
func (p Provider) Limit() float64 { return p.Monthly * float64(100-p.ReservePercent) / 100 }

// State is what collectors observed about one provider, plus what the
// controller remembers.
type State struct {
	Online, Registered bool
	// Idle is set when an online runner with the label has no job.
	Idle bool
	// UsedMinutes is a metered provider's billed minutes this month.
	UsedMinutes  float64
	OldestQueued time.Duration
	// Stalled is set after a queue stall until a runner with the label is online,
	// or until it is retried.
	Stalled bool
	// Paused holds why the user took the provider out of rotation.
	Paused string
}

// Decision is the label to write and why.
type Decision struct {
	Provider string
	Label    string
	Reason   string
}

// Usable reports whether p can take jobs in state s and why.
func Usable(p Provider, s State) (bool, string) {
	queueTimeout := p.QueueTimeout
	if queueTimeout == 0 {
		queueTimeout = MaxQueueWait
	}
	switch {
	case s.Paused != "":
		return false, s.Paused
	case p.Kind == Metered:
		left := p.Limit() - s.UsedMinutes
		if left <= 0 {
			return false, "free minutes at reserve"
		}
		return true, fmt.Sprintf("%.0f free minutes left", left)
	case s.OldestQueued > queueTimeout:
		return false, fmt.Sprintf("a job waited %s", s.OldestQueued.Round(time.Second))
	case s.Online:
		return true, "online"
	case s.Stalled:
		return false, "no runner since a job stalled"
	case s.Registered:
		return false, "offline"
	default:
		return true, "no runner registered; expecting runners per job"
	}
}

// Route is one repository variable and the providers that may serve it, in order.
type Route struct {
	Variable  string
	Providers []Provider
	// Fallback is chosen when nothing is usable, so jobs queue instead of costing money.
	Fallback Provider
}

// Decide returns the first usable provider of the route. Among usable metered
// providers it takes the one with the smallest share of its limit used, so every
// free tier lasts the month.
func Decide(rt Route, states map[string]State) Decision {
	var skipped []string
	for _, p := range rt.Providers {
		s, ok := states[p.Name]
		if !ok {
			skipped = append(skipped, p.Name+": not observed")
			continue
		}
		ok, why := Usable(p, s)
		if !ok {
			skipped = append(skipped, p.Name+": "+why)
			continue
		}
		if p.Kind == Metered {
			p, why = paced(rt.Providers, states)
		}
		return Decision{Provider: p.Name, Label: p.Label, Reason: reason(why, skipped)}
	}
	return Decision{Provider: rt.Fallback.Name, Label: rt.Fallback.Label, Reason: reason(fallbackReason(rt.Fallback), skipped)}
}

func paced(providers []Provider, states map[string]State) (Provider, string) {
	var best Provider
	bestShare := -1.0
	var shares []string
	for _, p := range providers {
		s, ok := states[p.Name]
		if !ok || p.Kind != Metered {
			continue
		}
		if usable, _ := Usable(p, s); !usable {
			continue
		}
		share := s.UsedMinutes / p.Limit()
		shares = append(shares, fmt.Sprintf("%s %.0f%%", p.Name, share*100))
		if bestShare < 0 || share < bestShare {
			best, bestShare = p, share
		}
	}
	if len(shares) == 1 {
		return best, fmt.Sprintf("%.0f free minutes left", best.Limit()-states[best.Name].UsedMinutes)
	}
	return best, "least used free tier: " + strings.Join(shares, ", ")
}

func fallbackReason(p Provider) string {
	if p.Kind == SelfHosted {
		return "no free capacity, jobs wait for your runner"
	}
	return "no free capacity, jobs queue on zerobill-wait; never assign this label to a runner"
}

func reason(chosen string, skipped []string) string {
	if len(skipped) == 0 {
		return chosen
	}
	return chosen + " (skipped " + strings.Join(skipped, "; ") + ")"
}

// Applied is the label last written for a route.
type Applied struct {
	Provider string    `json:"provider"`
	Label    string    `json:"label"`
	Since    time.Time `json:"since"`
}

// Hold keeps the applied label for MinHold while its provider stays usable, so a
// flapping runner does not bounce every queued job. It returns the decision to
// apply and whether it was held.
func Hold(d Decision, applied Applied, rt Route, states map[string]State, now time.Time) (Decision, bool) {
	if applied.Label == "" || applied.Label == d.Label || now.Sub(applied.Since) >= MinHold {
		return d, false
	}
	for _, p := range rt.Providers {
		if p.Name == d.Provider && p.Kind == SelfHosted {
			if s, ok := states[p.Name]; ok {
				if usable, _ := Usable(p, s); usable {
					return d, false
				}
			}
		}
	}
	for _, p := range rt.Providers {
		if p.Name != applied.Provider || p.Label != applied.Label {
			continue
		}
		s, ok := states[p.Name]
		if !ok {
			return d, false
		}
		if usable, why := Usable(p, s); usable {
			wait := (MinHold - now.Sub(applied.Since)).Round(time.Second)
			return Decision{Provider: p.Name, Label: p.Label, Reason: fmt.Sprintf("held %s for %s more; %s preferred: %s", why, wait, d.Provider, d.Reason)}, true
		}
		return d, false
	}
	return d, false
}
