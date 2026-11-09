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
	"time"

	"github.com/morhaf-ko/zerobill/internal/atomicfile"
	"gopkg.in/yaml.v3"
)

const (
	DefaultReservePercent        = 10
	DefaultAlertRemainingPercent = 20
	DefaultCheckInterval         = 5 * time.Minute
	DefaultQueueTimeout          = 10 * time.Minute
	// WaitingLabel must never be assigned to a runner; it queues jobs without selecting paid capacity.
	WaitingLabel = "zerobill-wait"
)

// Config selects available capacity and the controller's checking and warning thresholds.
type Config struct {
	Owner                 string        `yaml:"owner"`
	SelfHostedLabel       string        `yaml:"self_hosted_label,omitempty"`
	Blacksmith            bool          `yaml:"blacksmith,omitempty"`
	ReservePercent        int           `yaml:"reserve_percent"`
	AlertRemainingPercent int           `yaml:"alert_remaining_percent"`
	CheckInterval         time.Duration `yaml:"check_interval"`
	QueueTimeout          time.Duration `yaml:"queue_timeout"`
	Notify                string        `yaml:"notify,omitempty"`
}

func Defaults() Config {
	return Config{ReservePercent: DefaultReservePercent, AlertRemainingPercent: DefaultAlertRemainingPercent,
		CheckInterval: DefaultCheckInterval, QueueTimeout: DefaultQueueTimeout}
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
	c := Defaults()
	var file struct {
		Owner                 string        `yaml:"owner"`
		SelfHostedLabel       string        `yaml:"self_hosted_label"`
		Blacksmith            bool          `yaml:"blacksmith"`
		Notify                string        `yaml:"notify"`
		ReservePercent        yaml.Node     `yaml:"reserve_percent"`
		AlertRemainingPercent yaml.Node     `yaml:"alert_remaining_percent"`
		CheckInterval         time.Duration `yaml:"check_interval"`
		QueueTimeout          time.Duration `yaml:"queue_timeout"`
	}
	file.CheckInterval, file.QueueTimeout = c.CheckInterval, c.QueueTimeout
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		if strings.Contains(err.Error(), "field runner not found") {
			return nil, fmt.Errorf("%s: rename runner to self_hosted_label; it identifies your optional self-hosted GitHub Actions runners", path)
		}
		return nil, fmt.Errorf("%s: %w; see examples/config.yaml for supported settings", path, err)
	}
	c.Owner, c.SelfHostedLabel, c.Blacksmith, c.Notify = file.Owner, file.SelfHostedLabel, file.Blacksmith, file.Notify
	c.CheckInterval, c.QueueTimeout = file.CheckInterval, file.QueueTimeout
	for _, field := range []struct {
		name  string
		node  yaml.Node
		value *int
	}{
		{"reserve_percent", file.ReservePercent, &c.ReservePercent},
		{"alert_remaining_percent", file.AlertRemainingPercent, &c.AlertRemainingPercent},
	} {
		if field.node.Kind == 0 {
			continue
		}
		if field.node.Tag != "!!int" {
			return nil, fmt.Errorf("%s: %s must be an integer", path, field.name)
		}
		if err := field.node.Decode(field.value); err != nil {
			return nil, fmt.Errorf("%s: %s must be an integer", path, field.name)
		}
	}

	if err := c.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// FromEnv reads ZEROBILL_* settings for containers and reports false when
// ZEROBILL_OWNER is unset.
func FromEnv() (*Config, bool, error) {
	c := Defaults()
	c.Owner, c.SelfHostedLabel, c.Notify = os.Getenv("ZEROBILL_OWNER"), os.Getenv("ZEROBILL_SELF_HOSTED_LABEL"), os.Getenv("ZEROBILL_NOTIFY")
	if c.Owner == "" {
		return nil, false, nil
	}
	if os.Getenv("ZEROBILL_RUNNER") != "" {
		return nil, true, errors.New("rename ZEROBILL_RUNNER to ZEROBILL_SELF_HOSTED_LABEL")
	}
	for name, value := range map[string]*time.Duration{"ZEROBILL_CHECK_INTERVAL": &c.CheckInterval, "ZEROBILL_QUEUE_TIMEOUT": &c.QueueTimeout} {
		if v := os.Getenv(name); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, true, fmt.Errorf("%s: use a duration such as 1m or 5m", name)
			}
			*value = d
		}
	}
	if v := os.Getenv("ZEROBILL_ALERT_REMAINING_PERCENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, true, errors.New("ZEROBILL_ALERT_REMAINING_PERCENT must be an integer between 0 and 99")
		}
		c.AlertRemainingPercent = n
	}

	if v := os.Getenv("ZEROBILL_RESERVE_PERCENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, true, errors.New("ZEROBILL_RESERVE_PERCENT must be an integer between 0 and 99")
		}
		c.ReservePercent = n
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
	if c.AlertRemainingPercent < 0 || c.AlertRemainingPercent > 99 {
		return errors.New("alert_remaining_percent must be an integer between 0 and 99; zero disables warnings")
	}
	if c.SelfHostedLabel == WaitingLabel {
		return fmt.Errorf("self_hosted_label cannot be %s; this label is reserved for waiting jobs", WaitingLabel)
	}
	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{"check_interval", c.CheckInterval}, {"queue_timeout", c.QueueTimeout},
	} {
		if field.value < time.Minute || field.value > time.Hour {
			return fmt.Errorf("%s must be a duration between 1m and 1h", field.name)
		}
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
	var b bytes.Buffer
	raw, err := yaml.Marshal(map[string]string{"owner": c.Owner})
	if err != nil {
		return err
	}
	b.Write(raw)
	for _, field := range []struct {
		name      string
		value     any
		commented bool
		help      string
	}{
		{"self_hosted_label", defaultExample(c.SelfHostedLabel, "home-ci"), c.SelfHostedLabel == "", "Optional self-hosted GitHub Actions label. Find it in Settings > Actions > Runners.\nOnly include machines you want used automatically. Omit without your own runners."},
		{"blacksmith", c.Blacksmith, !c.Blacksmith, "Enable only after installing the Blacksmith GitHub App."},
		{"reserve_percent", c.ReservePercent, c.ReservePercent == DefaultReservePercent, "Stop selecting a hosted tier with this percentage of its full allowance left."},
		{"alert_remaining_percent", c.AlertRemainingPercent, c.AlertRemainingPercent == DefaultAlertRemainingPercent, "Warn at this percentage of the full allowance remaining. Zero disables warnings.\nChoose a value above reserve_percent for advance notice."},
		{"check_interval", c.CheckInterval.String(), c.CheckInterval == DefaultCheckInterval, "Check runner availability and usage this often. Shorter intervals use more GitHub API requests."},
		{"queue_timeout", c.QueueTimeout.String(), c.QueueTimeout == DefaultQueueTimeout, "Treat self-hosted capacity as unavailable after a job waits this long."},
		{"notify", defaultExample(c.Notify, "https://ntfy.sh/your-topic"), c.Notify == "", "Optional routing-change and low-capacity alerts: ntfy, Discord or Slack webhook."},
	} {
		fmt.Fprintf(&b, "\n# %s\n", strings.ReplaceAll(field.help, "\n", "\n# "))
		raw, err := yaml.Marshal(map[string]any{field.name: field.value})
		if err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
			if field.commented {
				b.WriteString("# ")
			}
			fmt.Fprintln(&b, line)
		}
	}
	return atomicfile.Create(path, b.Bytes())
}

func defaultExample(value, example string) string {
	if value == "" {
		return example
	}
	return value
}
