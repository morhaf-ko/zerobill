// Package config loads zerobill's YAML configuration and observed state.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/morhaf-ko/zerobill/internal/policy"
	"gopkg.in/yaml.v3"
)

// Modes. Only watch in switch mode writes to GitHub.
const (
	DryRun = "dry-run"
	Switch = "switch"
)

// DefaultMinHold applies when min_hold is unset.
const DefaultMinHold = 15 * time.Minute

// DefaultRetryAfter applies to on-demand providers without retry_after.
const DefaultRetryAfter = time.Hour

// DefaultRoute names the route built from top-level variables when routes is unset.
const DefaultRoute = "default"

// Config is the operator-owned routing policy.
type Config struct {
	Mode string `yaml:"mode"`
	// MinHold is how long an applied label stays before moving back to a
	// higher-priority provider.
	MinHold string `yaml:"min_hold"`
	minHold time.Duration
	// Owner is the organization or user whose runners, billing and repositories
	// the live collectors read.
	Owner        string   `yaml:"owner"`
	Repositories []string `yaml:"repositories"`
	// Variables and Fallback form the default route over every provider when
	// Routes is empty. With Routes, Fallback is each route's default fallback.
	Variables []string   `yaml:"variables"`
	Fallback  string     `yaml:"fallback"`
	Providers []Provider `yaml:"providers"`
	Routes    []Route    `yaml:"routes"`
}

// Route assigns variables to an ordered subset of providers.
type Route struct {
	Name      string   `yaml:"name"`
	Variables []string `yaml:"variables"`
	Providers []string `yaml:"providers"`
	Fallback  string   `yaml:"fallback"`
	Balance   string   `yaml:"balance"`
}

// Provider mirrors policy.Provider with YAML-friendly durations.
type Provider struct {
	Name           string  `yaml:"name"`
	Label          string  `yaml:"label"`
	Kind           string  `yaml:"kind"`
	MonthlyMinutes float64 `yaml:"monthly_minutes"`
	ReserveMinutes float64 `yaml:"reserve_minutes"`
	Multiplier     float64 `yaml:"multiplier"`
	MaxQueueWait   string  `yaml:"max_queue_wait"`
	OnDemand       bool    `yaml:"on_demand"`
	RetryAfter     string  `yaml:"retry_after"`
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.Mode == "" {
		c.Mode = DryRun
	}
	if c.Mode != DryRun && c.Mode != Switch {
		return fmt.Errorf("mode %q is not supported; use %s or %s", c.Mode, DryRun, Switch)
	}
	c.minHold = DefaultMinHold
	if c.MinHold != "" {
		d, err := time.ParseDuration(c.MinHold)
		if err != nil || d < 0 {
			return fmt.Errorf("min_hold %q must be a non-negative duration", c.MinHold)
		}
		c.minHold = d
	}
	if len(c.Providers) == 0 {
		return errors.New("at least one provider is required")
	}
	seen := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" || p.Label == "" {
			return errors.New("every provider needs a name and a label")
		}
		if seen[p.Name] {
			return fmt.Errorf("provider %s is listed twice", p.Name)
		}
		seen[p.Name] = true
		if p.Kind == string(policy.Metered) && p.MonthlyMinutes <= p.ReserveMinutes {
			return fmt.Errorf("metered provider %s needs monthly_minutes above reserve_minutes", p.Name)
		}
		if _, err := duration(p.MaxQueueWait, 0); err != nil {
			return fmt.Errorf("provider %s max_queue_wait %q: %w", p.Name, p.MaxQueueWait, err)
		}
		if _, err := duration(p.RetryAfter, 0); err != nil {
			return fmt.Errorf("provider %s retry_after %q: %w", p.Name, p.RetryAfter, err)
		}
		if p.OnDemand && (p.Kind != string(policy.SelfHosted) || p.MaxQueueWait == "") {
			return fmt.Errorf("on_demand provider %s must be self-hosted with max_queue_wait", p.Name)
		}
	}
	if len(c.Routes) == 0 {
		names := make([]string, len(c.Providers))
		for i, p := range c.Providers {
			names[i] = p.Name
		}
		c.Routes = []Route{{Name: DefaultRoute, Variables: c.Variables, Providers: names, Fallback: c.Fallback}}
	} else if len(c.Variables) > 0 {
		return errors.New("set variables inside routes, not at the top level, when routes is used")
	}
	routes, vars := map[string]bool{}, map[string]string{}
	for i := range c.Routes {
		rt := &c.Routes[i]
		if rt.Name == "" || routes[rt.Name] {
			return fmt.Errorf("route %d needs a unique name", i+1)
		}
		routes[rt.Name] = true
		if rt.Fallback == "" {
			rt.Fallback = c.Fallback
		}
		if !seen[rt.Fallback] {
			return fmt.Errorf("route %s: fallback %q is not a configured provider", rt.Name, rt.Fallback)
		}
		if rt.Balance == "" {
			rt.Balance = string(policy.Order)
		}
		if rt.Balance != string(policy.Order) && rt.Balance != string(policy.Pace) {
			return fmt.Errorf("route %s: balance %q is not supported; use %s or %s", rt.Name, rt.Balance, policy.Order, policy.Pace)
		}
		if len(rt.Providers) == 0 {
			return fmt.Errorf("route %s needs providers", rt.Name)
		}
		for _, p := range rt.Providers {
			if !seen[p] {
				return fmt.Errorf("route %s: provider %q is not configured", rt.Name, p)
			}
		}
		for _, v := range rt.Variables {
			if other, ok := vars[v]; ok {
				return fmt.Errorf("variable %s is in routes %s and %s", v, other, rt.Name)
			}
			vars[v] = rt.Name
		}
		if c.Mode == Switch && len(rt.Variables) == 0 {
			return fmt.Errorf("switch mode needs variables in route %s", rt.Name)
		}
	}
	if c.Mode == Switch {
		if len(c.Repositories) == 0 {
			return errors.New("switch mode needs repositories")
		}
		for _, r := range c.Repositories {
			if owner, name, ok := strings.Cut(r, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
				return fmt.Errorf("repository %q must be owner/name", r)
			}
		}
	}
	return nil
}

// Hold returns the parsed min_hold.
func (c *Config) Hold() time.Duration { return c.minHold }

// PolicyProviders converts the config into policy input, in priority order.
func (c *Config) PolicyProviders() ([]policy.Provider, error) {
	out := make([]policy.Provider, 0, len(c.Providers))
	for _, p := range c.Providers {
		wait, err := duration(p.MaxQueueWait, 0)
		if err != nil {
			return nil, fmt.Errorf("provider %s max_queue_wait: %w", p.Name, err)
		}
		retry, err := duration(p.RetryAfter, DefaultRetryAfter)
		if err != nil {
			return nil, fmt.Errorf("provider %s retry_after: %w", p.Name, err)
		}
		mult := p.Multiplier
		if mult == 0 {
			mult = 1
		}
		out = append(out, policy.Provider{
			Name: p.Name, Label: p.Label, Kind: policy.Kind(p.Kind),
			MonthlyMinutes: p.MonthlyMinutes, ReserveMinutes: p.ReserveMinutes,
			Multiplier: mult, MaxQueueWait: wait, OnDemand: p.OnDemand, RetryAfter: retry,
		})
	}
	return out, nil
}

// PolicyRoutes resolves each route's provider names against providers.
func (c *Config) PolicyRoutes(providers []policy.Provider) []policy.Route {
	byName := make(map[string]policy.Provider, len(providers))
	for _, p := range providers {
		byName[p.Name] = p
	}
	out := make([]policy.Route, 0, len(c.Routes))
	for _, rt := range c.Routes {
		pr := policy.Route{Name: rt.Name, Variables: rt.Variables, Fallback: byName[rt.Fallback], Balance: policy.Balance(rt.Balance)}
		for _, n := range rt.Providers {
			pr.Providers = append(pr.Providers, byName[n])
		}
		out = append(out, pr)
	}
	return out
}

func duration(v string, def time.Duration) (time.Duration, error) {
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err == nil && d < 0 {
		err = errors.New("must not be negative")
	}
	return d, err
}

// Observation is a snapshot of provider state, written by collectors or by hand.
type Observation struct {
	Providers map[string]Observed `yaml:"providers"`
}

// Observed is one provider's state. Self-hosted providers set Online and
// OldestQueued; metered providers set UsedMinutes.
type Observed struct {
	Online       *bool    `yaml:"online,omitempty" json:"online,omitempty"`
	UsedMinutes  *float64 `yaml:"used_minutes,omitempty" json:"used_minutes,omitempty"`
	OldestQueued string   `yaml:"oldest_queued,omitempty" json:"oldest_queued,omitempty"`
}

// Snapshot converts collected state into the snapshot file format.
func Snapshot(providers []policy.Provider, states map[string]policy.State) Observation {
	o := Observation{Providers: map[string]Observed{}}
	for _, p := range providers {
		s, ok := states[p.Name]
		if !ok {
			continue
		}
		var ob Observed
		if p.Kind == policy.SelfHosted {
			ob.Online = &s.Online
			if s.OldestQueued > 0 {
				ob.OldestQueued = s.OldestQueued.Round(time.Second).String()
			}
		} else {
			used := s.UsedMinutes
			ob.UsedMinutes = &used
		}
		o.Providers[p.Name] = ob
	}
	return o
}

// LoadObservation reads a snapshot file into policy state.
func LoadObservation(path string) (map[string]policy.State, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var o Observation
	if err := yaml.Unmarshal(raw, &o); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	states := make(map[string]policy.State, len(o.Providers))
	for name, p := range o.Providers {
		var q time.Duration
		if p.OldestQueued != "" {
			d, err := time.ParseDuration(p.OldestQueued)
			if err != nil {
				return nil, fmt.Errorf("%s: provider %s oldest_queued: %w", path, name, err)
			}
			q = d
		}
		s := policy.State{OldestQueued: q}
		if p.Online != nil {
			s.Online = *p.Online
		}
		if p.UsedMinutes != nil {
			s.UsedMinutes = *p.UsedMinutes
		}
		states[name] = s
	}
	return states, nil
}
