// Package config loads zerobill's three-line configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the user's choice of free capacity. Everything else is a convention.
type Config struct {
	// Owner is the organization or user whose CI zerobill routes.
	Owner string `yaml:"owner"`
	// Runner is the label of the user's self-hosted runners; empty without one.
	Runner string `yaml:"runner"`
	// Blacksmith adds Blacksmith's free minutes.
	Blacksmith bool `yaml:"blacksmith"`
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
		return nil, fmt.Errorf("no config at %s; create it with one line: owner: <your user or organization>", path)
	}
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		if strings.Contains(err.Error(), "not found in type") {
			return nil, fmt.Errorf("%s: %w; the config takes only owner, runner and blacksmith", path, err)
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Owner == "" {
		return nil, fmt.Errorf("%s: owner is required", path)
	}
	return &c, nil
}
