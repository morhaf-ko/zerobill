// Package config loads zerobill's configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/morhaf-ko/zerobill/internal/atomicfile"
	"gopkg.in/yaml.v3"
)

const DefaultReservePercent = 10

// Percent accepts only YAML integer scalars.
type Percent int

func (p *Percent) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag != "!!int" {
		return errors.New("reserve_percent must be an integer between 0 and 99")
	}
	var n int
	if err := node.Decode(&n); err != nil {
		return errors.New("reserve_percent must be an integer between 0 and 99")
	}
	*p = Percent(n)
	return nil
}

// Config is the user's choice of free capacity. Everything else is a convention.
type Config struct {
	// Owner is the organization or user whose CI zerobill routes.
	Owner string `yaml:"owner"`
	// Runner is the label of the user's self-hosted runners; empty without one.
	Runner string `yaml:"runner,omitempty"`
	// Blacksmith adds Blacksmith's free minutes.
	Blacksmith     bool    `yaml:"blacksmith,omitempty"`
	ReservePercent Percent `yaml:"reserve_percent"`
	// Notify is an ntfy topic, Discord or Slack webhook, or any URL that takes a
	// plain-text POST, told whenever zerobill changes its choice.
	Notify string `yaml:"notify,omitempty"`
}

// Path returns the config file: $XDG_CONFIG_HOME/zerobill/config.yaml, else
// ~/.config/zerobill/config.yaml on every platform, as the GitHub CLI does.
func Path() (string, error) {
	return xdg("XDG_CONFIG_HOME", ".config", "config.yaml")
}

// StateDir returns $XDG_STATE_HOME/zerobill, else ~/.local/state/zerobill.
func StateDir() (string, error) {
	return xdg("XDG_STATE_HOME", filepath.Join(".local", "state"), "")
}

func xdg(env, fallback, file string) (string, error) {
	base := os.Getenv(env)
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, fallback)
	}
	return filepath.Join(base, "zerobill", file), nil
}

// Load reads and checks the config at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no config at %s; run zerobill init", path)
	}
	if err != nil {
		return nil, err
	}
	c := Config{ReservePercent: DefaultReservePercent}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		if strings.Contains(err.Error(), "not found in type") {
			return nil, fmt.Errorf("%s: %w; the config takes only owner, runner, blacksmith, reserve_percent and notify", path, err)
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// FromEnv reads ZEROBILL_* settings for containers and reports false when
// ZEROBILL_OWNER is unset.
func FromEnv() (*Config, bool, error) {
	c := Config{Owner: os.Getenv("ZEROBILL_OWNER"), Runner: os.Getenv("ZEROBILL_RUNNER"), Notify: os.Getenv("ZEROBILL_NOTIFY"), ReservePercent: DefaultReservePercent}
	if c.Owner == "" {
		return nil, false, nil
	}
	if v := os.Getenv("ZEROBILL_RESERVE_PERCENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, true, errors.New("ZEROBILL_RESERVE_PERCENT must be an integer between 0 and 99")
		}
		c.ReservePercent = Percent(n)
	}
	switch v := os.Getenv("ZEROBILL_BLACKSMITH"); v {
	case "", "false":
	case "true":
		c.Blacksmith = true
	default:
		return nil, true, fmt.Errorf("ZEROBILL_BLACKSMITH is %q; use true or false", v)
	}
	if err := c.check(); err != nil {
		return nil, true, fmt.Errorf("environment: %w", err)
	}
	return &c, true, nil
}

func (c *Config) check() error {
	if c.Owner == "" {
		return errors.New("owner is required")
	}
	if c.ReservePercent < 0 || c.ReservePercent > 99 {
		return errors.New("reserve_percent must be an integer between 0 and 99")
	}
	if u, err := url.Parse(c.Notify); c.Notify != "" && (err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "") {
		return errors.New("notify must be an http or https URL")
	}
	return nil
}

// Create writes a validated config without replacing an existing file or symlink.
func Create(path string, c Config) error {
	if err := c.check(); err != nil {
		return err
	}
	raw, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return atomicfile.Create(path, raw)
}
