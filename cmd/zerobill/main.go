// Command zerobill decides which runner label keeps CI inside free capacity.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/morhaf-ko/zerobill/internal/apply"
	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/config"
	"github.com/morhaf-ko/zerobill/internal/github"
	"github.com/morhaf-ko/zerobill/internal/policy"
	"gopkg.in/yaml.v3"
)

const usage = `usage:
  zerobill plan --config zerobill.yaml --observed observed.yaml
  zerobill observe --config zerobill.yaml [--state zerobill-state.json] [--out observed.yaml]
  zerobill watch --config zerobill.yaml [--state zerobill-state.json] [--applied zerobill-applied.json] [--interval 5m] [--count 0]

observe and watch read GITHUB_TOKEN. Only watch with mode: switch writes to GitHub,
and it writes nothing in an interval where any collector fails. observe fails when
any collector fails; watch logs the error.`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "plan":
		err = plan(os.Args[2:])
	case "observe":
		err = observe(os.Args[2:])
	case "watch":
		err = watch(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "zerobill:", err)
		os.Exit(1)
	}
}

func plan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	cfgPath := fs.String("config", "zerobill.yaml", "routing policy")
	obsPath := fs.String("observed", "", "provider state snapshot")
	_ = fs.Parse(args)
	if *obsPath == "" {
		return errors.New("--observed is required; use watch for live state")
	}
	cfg, providers, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	states, err := config.LoadObservation(*obsPath)
	if err != nil {
		return err
	}
	d, err := policy.Decide(providers, states, cfg.Fallback)
	if err != nil {
		return err
	}
	fmt.Printf("mode: %s\n", cfg.Mode)
	fmt.Printf("decision: %s -> %s\n", d.Provider, d.Label)
	fmt.Printf("reason: %s\n", d.Reason)
	for _, repo := range cfg.Repositories {
		fmt.Printf("would set %s on %s to %s\n", strings.Join(cfg.Variables, ", "), repo, d.Label)
	}
	return nil
}

func observe(args []string) error {
	fs := flag.NewFlagSet("observe", flag.ExitOnError)
	cfgPath := fs.String("config", "zerobill.yaml", "routing policy")
	statePath := fs.String("state", "zerobill-state.json", "Blacksmith job cache")
	out := fs.String("out", "", "snapshot file; stdout when empty")
	_ = fs.Parse(args)
	cfg, providers, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	c, err := newCollector(cfg, *statePath)
	if err != nil {
		return err
	}
	r := c.Observe(providers)
	if len(r.Errors) > 0 {
		return fmt.Errorf("collect: %s", joinErrors(r.Errors))
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(config.Snapshot(providers, r.States)); err != nil {
		return err
	}
	if *out == "" {
		_, err = os.Stdout.Write(buf.Bytes())
		return err
	}
	return os.WriteFile(*out, buf.Bytes(), 0o644)
}

func watch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	cfgPath := fs.String("config", "zerobill.yaml", "routing policy")
	statePath := fs.String("state", "zerobill-state.json", "Blacksmith job cache")
	appliedPath := fs.String("applied", "zerobill-applied.json", "last applied label, used in switch mode")
	interval := fs.Duration("interval", 5*time.Minute, "time between decisions")
	count := fs.Int("count", 0, "stop after this many decisions; 0 runs forever")
	_ = fs.Parse(args)
	if *interval <= 0 {
		return errors.New("--interval must be positive")
	}
	cfg, providers, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	c, err := newCollector(cfg, *statePath)
	if err != nil {
		return err
	}
	ctl := &controller{cfg: cfg, providers: providers}
	if cfg.Mode == config.Switch {
		ctl.writer = &apply.Writer{GH: c.GH}
		ctl.appliedPath = *appliedPath
		if ctl.applied, err = apply.LoadApplied(*appliedPath); err != nil {
			return err
		}
	}
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	for i := 1; ; i++ {
		r := c.Observe(providers)
		if err := ctl.step(os.Stdout, time.Now().UTC(), r); err != nil {
			return err
		}
		if *count > 0 && i >= *count {
			return nil
		}
		<-tick.C
	}
}

// decisionLine is one watch log entry.
type decisionLine struct {
	Time         time.Time                  `json:"time"`
	Mode         string                     `json:"mode"`
	Provider     string                     `json:"provider,omitempty"`
	Label        string                     `json:"label,omitempty"`
	Reason       string                     `json:"reason,omitempty"`
	Fallback     bool                       `json:"fallback"`
	Held         bool                       `json:"held,omitempty"`
	Observed     map[string]config.Observed `json:"observed"`
	Errors       map[string]string          `json:"errors,omitempty"`
	Repositories []string                   `json:"repositories"`
	Variables    []string                   `json:"variables"`
	// Written lists "owner/name VARIABLE" pairs changed in this interval; null in
	// dry-run mode and when nothing was attempted.
	Written []string `json:"written"`
}

// controller decides each interval and, with a writer, applies the decision.
type controller struct {
	cfg         *config.Config
	providers   []policy.Provider
	writer      *apply.Writer
	appliedPath string
	applied     policy.Applied
}

func (c *controller) step(w io.Writer, now time.Time, r collect.Result) error {
	line := decisionLine{
		Time: now, Mode: c.cfg.Mode,
		Observed:     config.Snapshot(c.providers, r.States).Providers,
		Errors:       map[string]string{},
		Repositories: c.cfg.Repositories, Variables: c.cfg.Variables,
	}
	for k, v := range r.Errors {
		line.Errors[k] = v
	}
	d, err := policy.Decide(c.providers, r.States, c.cfg.Fallback)
	if err != nil {
		line.Errors["policy"] = err.Error()
	} else if c.writer != nil && len(r.Errors) > 0 {
		line.Errors["write"] = "skipped: a collector failed"
	} else if c.writer != nil {
		d, line.Held = policy.Hold(d, c.applied, c.providers, r.States, now, c.cfg.Hold())
		written, werr := c.writer.Sync(c.cfg.Repositories, c.cfg.Variables, d.Label)
		line.Written = written
		if line.Written == nil {
			line.Written = []string{}
		}
		if werr != nil {
			line.Errors["write"] = werr.Error()
		} else if d.Label != c.applied.Label || d.Provider != c.applied.Provider {
			c.applied = policy.Applied{Provider: d.Provider, Label: d.Label, Since: now}
			if err := apply.SaveApplied(c.appliedPath, c.applied); err != nil {
				return err
			}
		}
	}
	if err == nil {
		line.Provider, line.Label, line.Reason, line.Fallback = d.Provider, d.Label, d.Reason, d.Fallback
	}
	if len(line.Errors) == 0 {
		line.Errors = nil
	}
	return json.NewEncoder(w).Encode(line)
}

func loadConfig(path string) (*config.Config, []policy.Provider, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, err
	}
	providers, err := cfg.PolicyProviders()
	if err != nil {
		return nil, nil, err
	}
	return cfg, providers, nil
}

func newCollector(cfg *config.Config, statePath string) (*collect.Collector, error) {
	if cfg.Owner == "" {
		return nil, errors.New("config owner is required for live collection")
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, errors.New("GITHUB_TOKEN is not set")
	}
	return &collect.Collector{
		GH: github.New(token), Owner: cfg.Owner,
		Repositories: cfg.Repositories, StatePath: statePath,
	}, nil
}

func joinErrors(errs map[string]string) string {
	names := make([]string, 0, len(errs))
	for n := range errs {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = n + ": " + errs[n]
	}
	return strings.Join(parts, "; ")
}
