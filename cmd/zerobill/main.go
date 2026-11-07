// Command zerobill decides which runner label keeps CI inside free capacity.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/morhaf-ko/zerobill/internal/apply"
	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/config"
	"github.com/morhaf-ko/zerobill/internal/github"
	"github.com/morhaf-ko/zerobill/internal/override"
	"github.com/morhaf-ko/zerobill/internal/policy"
	"gopkg.in/yaml.v3"
)

// version is set at release build time; go install builds report the module version.
var version = "dev"

func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}

const usage = `usage:
  zerobill version
  zerobill init --owner OWNER [--out zerobill.yaml] [--force]
  zerobill doctor --config zerobill.yaml
  zerobill plan --config zerobill.yaml --observed observed.yaml
  zerobill status --config zerobill.yaml [--state zerobill-state.json] [--applied zerobill-applied.json] [--overrides zerobill-overrides.json]
  zerobill observe --config zerobill.yaml [--state zerobill-state.json] [--out observed.yaml]
  zerobill watch --config zerobill.yaml [--state zerobill-state.json] [--applied zerobill-applied.json] [--overrides zerobill-overrides.json] [--interval 5m] [--count 0]
  zerobill disable PROVIDER [--for 72h] [--config zerobill.yaml] [--overrides zerobill-overrides.json]
  zerobill enable PROVIDER [--overrides zerobill-overrides.json]

init, doctor, status, observe and watch read GITHUB_TOKEN. Only watch with mode: switch writes to GitHub,
and it writes nothing in an interval where any collector fails. observe fails when
any collector fails; watch logs the error. disable and enable take a provider out of
rotation and back; a running watch reads the change on its next interval.`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "version", "--version":
		fmt.Println(buildVersion())
		return
	case "plan":
		err = plan(os.Args[2:])
	case "observe":
		err = observe(os.Args[2:])
	case "status":
		err = status(os.Args[2:])
	case "init":
		err = initConfig(os.Args[2:])
	case "doctor":
		err = doctor(os.Args[2:])
	case "watch":
		err = watch(os.Args[2:])
	case "disable":
		err = disable(os.Args[2:], time.Now().UTC())
	case "enable":
		err = enable(os.Args[2:])
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
	fmt.Printf("mode: %s\n", cfg.Mode)
	for _, rt := range cfg.PolicyRoutes(providers) {
		d, err := policy.Decide(rt, states)
		if err != nil {
			return err
		}
		fmt.Printf("route %s: %s -> %s\n", rt.Name, d.Provider, d.Label)
		fmt.Printf("  reason: %s\n", d.Reason)
		for _, repo := range cfg.Repositories {
			if len(rt.Variables) > 0 {
				fmt.Printf("  would set %s on %s to %s\n", strings.Join(rt.Variables, ", "), repo, d.Label)
			}
		}
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
	if len(r.Warnings) > 0 {
		fmt.Fprintln(os.Stderr, "zerobill:", joinErrors(r.Warnings))
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
	appliedPath := fs.String("applied", "zerobill-applied.json", "switch-mode state: applied labels, stalls and moved runs")
	overridesPath := fs.String("overrides", "zerobill-overrides.json", "providers disabled with zerobill disable")
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
	ctl := &controller{cfg: cfg, providers: providers, routes: cfg.PolicyRoutes(providers), overridesPath: *overridesPath}
	if cfg.Mode == config.Switch {
		ctl.writer = &apply.Writer{GH: c.GH}
		ctl.appliedPath = *appliedPath
		if ctl.state, err = apply.LoadState(*appliedPath); err != nil {
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
	Time     time.Time                  `json:"time"`
	Mode     string                     `json:"mode"`
	Routes   []routeLine                `json:"routes"`
	Observed map[string]config.Observed `json:"observed"`
	Disabled map[string]string          `json:"disabled,omitempty"`
	Errors   map[string]string          `json:"errors,omitempty"`
	Warnings map[string]string          `json:"warnings,omitempty"`
	// SavedUSD estimates what this month's self-hosted minutes would have cost on
	// GitHub-hosted runners.
	SavedUSD     float64  `json:"saved_usd_estimate"`
	Repositories []string `json:"repositories"`
	// Cancelled and Rerun list runs moved off a stale self-hosted label, as owner/name#id.
	Cancelled []string `json:"cancelled,omitempty"`
	Rerun     []string `json:"rerun,omitempty"`
}

// routeLine is one route's decision in a log entry.
type routeLine struct {
	Name      string   `json:"name"`
	Variables []string `json:"variables"`
	Provider  string   `json:"provider,omitempty"`
	Label     string   `json:"label,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Fallback  bool     `json:"fallback"`
	Held      bool     `json:"held,omitempty"`
	// Written lists "owner/name VARIABLE" pairs changed in this interval; null in
	// dry-run mode and when nothing was attempted.
	Written []string `json:"written"`
}

// controller decides each interval and, with a writer, applies the decision. In
// dry-run mode it keeps the same state in memory, so holds show in the log.
type controller struct {
	cfg           *config.Config
	providers     []policy.Provider
	routes        []policy.Route
	writer        *apply.Writer
	appliedPath   string
	overridesPath string
	state         apply.State
}

func (c *controller) step(w io.Writer, now time.Time, r collect.Result) error {
	line := decisionLine{
		Time: now, Mode: c.cfg.Mode,
		Observed:     config.Snapshot(c.providers, r.States).Providers,
		Errors:       map[string]string{},
		Repositories: c.cfg.Repositories,
	}
	for k, v := range r.Errors {
		line.Errors[k] = v
	}
	if len(r.Warnings) > 0 {
		line.Warnings = r.Warnings
	}
	line.SavedUSD = math.Round(policy.Saved(c.providers, r.States)*100) / 100
	states := c.adjust(now, r.States, line.Errors)
	for name, s := range states {
		if s.Disabled != "" {
			if line.Disabled == nil {
				line.Disabled = map[string]string{}
			}
			line.Disabled[name] = s.Disabled
		}
	}
	if c.state.Routes == nil {
		c.state.Routes = map[string]policy.Applied{}
	}
	write := c.writer != nil && len(r.Errors) == 0
	if c.writer != nil && !write {
		line.Errors["write"] = "skipped: a collector failed"
	}
	allWritten := true
	labels := map[string]bool{}
	for _, rt := range c.routes {
		rl := routeLine{Name: rt.Name, Variables: rt.Variables}
		d, err := policy.Decide(rt, states)
		if err != nil {
			line.Errors["policy "+rt.Name] = err.Error()
			allWritten = false
			line.Routes = append(line.Routes, rl)
			continue
		}
		applied := c.state.Routes[rt.Name]
		d, rl.Held = policy.Hold(d, applied, rt, states, now, c.cfg.Hold())
		rl.Provider, rl.Label, rl.Reason, rl.Fallback = d.Provider, d.Label, d.Reason, d.Fallback
		labels[d.Label] = true
		changed := d.Label != applied.Label || d.Provider != applied.Provider
		switch {
		case c.writer == nil:
			if changed {
				c.state.Routes[rt.Name] = policy.Applied{Provider: d.Provider, Label: d.Label, Since: now}
			}
		case write:
			written, werr := c.writer.Sync(c.cfg.Repositories, rt.Variables, d.Label)
			rl.Written = written
			if rl.Written == nil {
				rl.Written = []string{}
			}
			if werr != nil {
				line.Errors["write "+rt.Name] = werr.Error()
				allWritten = false
			}
			// A partial write still moves jobs, so it starts the hold for the new label.
			if changed && (werr == nil || len(written) > 0) {
				c.state.Routes[rt.Name] = policy.Applied{Provider: d.Provider, Label: d.Label, Since: now}
			}
		}
		line.Routes = append(line.Routes, rl)
	}
	if write {
		// Runs move only once every variable holds its new label, so a rerun job reads it.
		if allWritten {
			var merr error
			checkpoint := func() error { return apply.SaveState(c.appliedPath, c.state) }
			line.Cancelled, line.Rerun, merr = c.writer.Move(&c.state, c.stuck(labels, states, r), now, checkpoint)
			if merr != nil {
				line.Errors["move"] = merr.Error()
			}
		}
	}
	if c.writer != nil {
		if err := apply.SaveState(c.appliedPath, c.state); err != nil {
			line.Errors["state"] = err.Error()
		}
	}
	if len(line.Errors) == 0 {
		line.Errors = nil
	}
	return json.NewEncoder(w).Encode(line)
}

// adjust adds what the controller remembers to the observed states: operator
// overrides and the stall latch of on-demand providers. Problems reading
// overrides are reported in errs and leave every provider enabled.
func (c *controller) adjust(now time.Time, observed map[string]policy.State, errs map[string]string) map[string]policy.State {
	var disabled map[string]string
	if c.overridesPath != "" {
		set, err := override.Load(c.overridesPath)
		if err != nil {
			errs["overrides"] = err.Error()
		}
		disabled = set.Active(now)
	}
	if c.state.Stalled == nil {
		c.state.Stalled = map[string]time.Time{}
	}
	states := make(map[string]policy.State, len(observed))
	for _, p := range c.providers {
		s, ok := observed[p.Name]
		if !ok {
			continue
		}
		if p.OnDemand {
			switch {
			case s.Online:
				delete(c.state.Stalled, p.Name)
			case s.OldestQueued > p.MaxQueueWait:
				c.state.Stalled[p.Name] = now
			}
			if at, ok := c.state.Stalled[p.Name]; ok {
				if now.Sub(at) < p.RetryAfter {
					s.Stalled = true
				} else {
					delete(c.state.Stalled, p.Name)
				}
			}
		}
		s.Disabled = disabled[p.Name]
		states[p.Name] = s
	}
	return states
}

// stuck returns movable runs queued on self-hosted labels that no route chose
// whose provider cannot take jobs now.
func (c *controller) stuck(chosen map[string]bool, states map[string]policy.State, r collect.Result) []collect.Run {
	var runs []collect.Run
	for _, p := range c.providers {
		if p.Kind != policy.SelfHosted || chosen[p.Label] {
			continue
		}
		s, ok := states[p.Name]
		if !ok {
			continue
		}
		if usable, _, err := policy.Usable(p, s); err != nil || usable {
			continue
		}
		runs = append(runs, r.Movable[p.Name]...)
	}
	return runs
}

func disable(args []string, now time.Time) error {
	fs := flag.NewFlagSet("disable", flag.ExitOnError)
	cfgPath := fs.String("config", "zerobill.yaml", "routing policy, to check the provider name")
	overridesPath := fs.String("overrides", "zerobill-overrides.json", "providers disabled with zerobill disable")
	dur := fs.Duration("for", 0, "how long to disable; 0 disables until zerobill enable")
	name, err := providerArg(fs, args)
	if err != nil {
		return err
	}
	if *dur < 0 {
		return errors.New("--for must not be negative")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(cfg.Providers, func(p config.Provider) bool { return p.Name == name }) {
		return fmt.Errorf("provider %q is not in %s", name, *cfgPath)
	}
	set, err := override.Load(*overridesPath)
	if err != nil {
		return err
	}
	if set.Disabled == nil {
		set.Disabled = map[string]override.Entry{}
	}
	e := override.Entry{Since: now}
	if *dur > 0 {
		e.Until = now.Add(*dur)
	}
	set.Disabled[name] = e
	if err := override.Save(*overridesPath, set); err != nil {
		return err
	}
	fmt.Println(set.Active(now)[name] + ": " + name)
	return nil
}

func enable(args []string) error {
	fs := flag.NewFlagSet("enable", flag.ExitOnError)
	overridesPath := fs.String("overrides", "zerobill-overrides.json", "providers disabled with zerobill disable")
	name, err := providerArg(fs, args)
	if err != nil {
		return err
	}
	set, err := override.Load(*overridesPath)
	if err != nil {
		return err
	}
	if _, ok := set.Disabled[name]; !ok {
		fmt.Println("not disabled: " + name)
		return nil
	}
	delete(set.Disabled, name)
	if err := override.Save(*overridesPath, set); err != nil {
		return err
	}
	fmt.Println("enabled: " + name)
	return nil
}

// providerArg parses flags given before or after the provider name.
func providerArg(fs *flag.FlagSet, args []string) (string, error) {
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	_ = fs.Parse(args)
	if name == "" && fs.NArg() == 1 {
		name = fs.Arg(0)
	} else if fs.NArg() > 0 {
		return "", errors.New("give exactly one provider name")
	}
	if name == "" {
		return "", errors.New("give a provider name")
	}
	return name, nil
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
