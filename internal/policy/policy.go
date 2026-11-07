// Package policy picks the runner label that keeps CI inside free capacity.
package policy

import (
	"fmt"
	"slices"
	"strings"
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
	// MaxQueueWait marks a self-hosted provider saturated when its oldest queued job
	// has waited longer. Zero disables the check.
	MaxQueueWait time.Duration
	// OnDemand marks a self-hosted provider whose runners are created per job, such
	// as GARM or Actions Runner Controller pools, so no registered runner is not a
	// sign of an outage. Only a queue stall marks it unusable.
	OnDemand bool
	// RetryAfter is how long an on-demand provider stays unusable after a queue
	// stall unless a runner with its label comes online.
	RetryAfter time.Duration
}

// State is what collectors observed about one provider, plus what the controller
// remembers about it.
type State struct {
	Online       bool
	UsedMinutes  float64
	OldestQueued time.Duration
	// Stalled is set while an on-demand provider is inside RetryAfter of a queue stall.
	Stalled bool
	// Disabled holds the operator's reason for taking the provider out of rotation.
	Disabled string
}

// Decision is the label to write and why.
type Decision struct {
	Provider string
	Label    string
	Reason   string
	Fallback bool
}

// GitHubMinuteUSD is GitHub's price for a Linux 2-core hosted minute, which a
// minute served by a self-hosted runner would otherwise cost.
const GitHubMinuteUSD = 0.006

// Saved estimates what this month's self-hosted minutes would cost on
// GitHub-hosted runners.
func Saved(providers []Provider, states map[string]State) float64 {
	var minutes float64
	for _, p := range providers {
		if p.Kind == SelfHosted {
			minutes += states[p.Name].UsedMinutes
		}
	}
	return minutes * GitHubMinuteUSD
}

// Remaining returns billed minutes left before the reserve.
func (p Provider) Remaining(s State) float64 {
	return p.MonthlyMinutes - s.UsedMinutes - p.ReserveMinutes
}

// Usable reports whether p can take jobs in state s and why.
func Usable(p Provider, s State) (bool, string, error) {
	if s.Disabled != "" {
		return false, s.Disabled, nil
	}
	switch p.Kind {
	case SelfHosted:
		if p.MaxQueueWait > 0 && s.OldestQueued > p.MaxQueueWait {
			return false, fmt.Sprintf("queue waited %s", s.OldestQueued.Round(time.Second)), nil
		}
		if s.Online {
			return true, "online", nil
		}
		if !p.OnDemand {
			return false, "offline", nil
		}
		if s.Stalled {
			return false, "no runner since a queue stall", nil
		}
		return true, "on demand, no queue stall", nil
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

// Balance chooses between usable metered providers of a route.
type Balance string

const (
	// Order takes the first usable provider.
	Order Balance = "order"
	// Pace takes, among the usable metered providers, the one that has used the
	// smallest share of its minutes above the reserve, so all of them last the month.
	Pace Balance = "pace"
)

// Route is one set of variables and the providers that may serve them, in priority order.
type Route struct {
	Name      string
	Variables []string
	Providers []Provider
	// Fallback is chosen when no provider is usable, so jobs queue instead of
	// spending money. It need not be one of Providers.
	Fallback Provider
	Balance  Balance
}

// Decide returns the first usable provider of the route, balanced by its Balance.
// When none is usable it returns the route's fallback.
func Decide(rt Route, states map[string]State) (Decision, error) {
	var skipped []string
	for _, p := range rt.Providers {
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
		if p.Kind == Metered && rt.Balance == Pace {
			p, why = paced(rt.Providers, states)
		}
		return Decision{Provider: p.Name, Label: p.Label, Reason: reason(why, skipped)}, nil
	}
	return Decision{Provider: rt.Fallback.Name, Label: rt.Fallback.Label, Reason: reason("no free capacity, jobs queue", skipped), Fallback: true}, nil
}

// paced returns the usable metered provider with the smallest used share of its
// minutes above the reserve. Callers ensure at least one is usable.
func paced(providers []Provider, states map[string]State) (Provider, string) {
	var best Provider
	bestShare := -1.0
	var shares []string
	for _, p := range providers {
		s, ok := states[p.Name]
		if !ok || p.Kind != Metered {
			continue
		}
		if usable, _, err := Usable(p, s); err != nil || !usable {
			continue
		}
		share := s.UsedMinutes / (p.MonthlyMinutes - p.ReserveMinutes)
		shares = append(shares, fmt.Sprintf("%s %.0f%%", p.Name, share*100))
		if bestShare < 0 || share < bestShare {
			best, bestShare = p, share
		}
	}
	return best, "lowest share of free minutes used: " + strings.Join(shares, ", ")
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

// Hold keeps the applied label when d would move to another provider before
// minHold has passed. A move away from an applied provider that is no longer
// usable is never held. It returns the decision to apply and whether it was held.
func Hold(d Decision, applied Applied, rt Route, states map[string]State, now time.Time, minHold time.Duration) (Decision, bool) {
	if applied.Label == "" || applied.Label == d.Label || now.Sub(applied.Since) >= minHold {
		return d, false
	}
	for _, p := range slices.Concat(rt.Providers, []Provider{rt.Fallback}) {
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
