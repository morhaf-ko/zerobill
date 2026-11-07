// Package config loads zerobill's YAML configuration and observed state.
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/morhaf-ko/zerobill/internal/policy"
	"gopkg.in/yaml.v3"
)

// Config is the operator-owned routing policy.
type Config struct {
	Mode string `yaml:"mode"`
	// Owner is the organization or user whose runners, billing and repositories
	// the live collectors read.
	Owner        string     `yaml:"owner"`
	Repositories []string   `yaml:"repositories"`
	Variables    []string   `yaml:"variables"`
	Fallback     string     `yaml:"fallback"`
	Providers    []Provider `yaml:"providers"`
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
		c.Mode = "dry-run"
	}
	if c.Mode != "dry-run" {
		return fmt.Errorf("mode %q is not supported yet; only dry-run", c.Mode)
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
		if p.Kind == string(policy.Metered) && p.MonthlyMinutes <= 0 {
			return fmt.Errorf("metered provider %s needs monthly_minutes", p.Name)
		}
	}
	if !seen[c.Fallback] {
		return fmt.Errorf("fallback %q is not a configured provider", c.Fallback)
	}
	return nil
}

// PolicyProviders converts the config into policy input, in priority order.
func (c *Config) PolicyProviders() ([]policy.Provider, error) {
	out := make([]policy.Provider, 0, len(c.Providers))
	for _, p := range c.Providers {
		var wait time.Duration
		if p.MaxQueueWait != "" {
			d, err := time.ParseDuration(p.MaxQueueWait)
			if err != nil {
				return nil, fmt.Errorf("provider %s max_queue_wait: %w", p.Name, err)
			}
			wait = d
		}
		mult := p.Multiplier
		if mult == 0 {
			mult = 1
		}
		out = append(out, policy.Provider{
			Name: p.Name, Label: p.Label, Kind: policy.Kind(p.Kind),
			MonthlyMinutes: p.MonthlyMinutes, ReserveMinutes: p.ReserveMinutes,
			Multiplier: mult, MaxQueueWait: wait,
		})
	}
	return out, nil
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
