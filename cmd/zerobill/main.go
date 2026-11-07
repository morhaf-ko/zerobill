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

	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/config"
	"github.com/morhaf-ko/zerobill/internal/github"
	"github.com/morhaf-ko/zerobill/internal/policy"
	"gopkg.in/yaml.v3"
)

const usage = `usage:
  zerobill plan --config zerobill.yaml --observed observed.yaml
  zerobill observe --config zerobill.yaml [--state zerobill-state.json] [--out observed.yaml]
  zerobill watch --config zerobill.yaml [--state zerobill-state.json] [--interval 5m] [--count 0]

observe and watch read GITHUB_TOKEN and never write to GitHub.`

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
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	for i := 1; ; i++ {
		if err := logDecision(os.Stdout, time.Now().UTC(), cfg, providers, c.Observe(providers)); err != nil {
			return err
		}
		if *count > 0 && i >= *count {
			return nil
		}
		<-tick.C
	}
}

// decisionLine is one watch log entry. Nothing in it is written to GitHub.
type decisionLine struct {
	Time         time.Time                  `json:"time"`
	Mode         string                     `json:"mode"`
	Provider     string                     `json:"provider,omitempty"`
	Label        string                     `json:"label,omitempty"`
	Reason       string                     `json:"reason,omitempty"`
	Fallback     bool                       `json:"fallback"`
	Observed     map[string]config.Observed `json:"observed"`
	Errors       map[string]string          `json:"errors,omitempty"`
	Repositories []string                   `json:"would_set_repositories"`
	Variables    []string                   `json:"would_set_variables"`
}

func logDecision(w io.Writer, now time.Time, cfg *config.Config, providers []policy.Provider, r collect.Result) error {
	line := decisionLine{
		Time: now, Mode: cfg.Mode,
		Observed:     config.Snapshot(providers, r.States).Providers,
		Errors:       r.Errors,
		Repositories: cfg.Repositories, Variables: cfg.Variables,
	}
	d, err := policy.Decide(providers, r.States, cfg.Fallback)
	if err != nil {
		if line.Errors == nil {
			line.Errors = map[string]string{}
		}
		line.Errors["policy"] = err.Error()
	} else {
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
