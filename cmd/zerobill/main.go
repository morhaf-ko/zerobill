// Command zerobill keeps GitHub Actions jobs on free runners.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/config"
	"github.com/morhaf-ko/zerobill/internal/github"
	"github.com/morhaf-ko/zerobill/internal/override"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

// version is set at release build time; go install builds report the module version.
var version = "dev"

func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}

const usage = `zerobill keeps GitHub Actions jobs on free runners.

usage:
  zerobill [status]                     free minutes, current choices and problems
  zerobill run [--apply]                keep CI_RUNNER and BOT_RUNNER on the best free
                                        runner; without --apply it only logs
  zerobill pause runner|blacksmith|github [duration]
  zerobill resume runner|blacksmith|github
  zerobill version

Config: ~/.config/zerobill/config.yaml with owner, and optionally runner and blacksmith.
GitHub access: GITHUB_TOKEN, else the GitHub CLI login (gh auth token).`

func main() {
	cmd, args := "status", os.Args[1:]
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "status":
		err = status(args)
	case "run":
		err = run(args)
	case "pause":
		err = pause(args, time.Now().UTC())
	case "resume":
		err = resume(args)
	case "version", "--version":
		fmt.Println(buildVersion())
	case "help", "-h", "--help":
		fmt.Println(usage)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "zerobill:", err)
		os.Exit(1)
	}
}

// app is what every command needs: the config, GitHub access and the providers
// and routes the config implies.
type app struct {
	cfg       *config.Config
	gh        *github.Client
	col       *collect.Collector
	org       bool
	stateDir  string
	providers []policy.Provider
	routes    []policy.Route
}

func load() (*app, error) {
	path, err := config.Path()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	stateDir, err := config.StateDir()
	if err != nil {
		return nil, err
	}
	token, err := githubToken()
	if err != nil {
		return nil, err
	}
	gh := github.New(token)
	col := &collect.Collector{GH: gh, Owner: cfg.Owner, StatePath: filepath.Join(stateDir, "cache.json")}
	org, err := col.OwnerIsOrg()
	if err != nil {
		return nil, fmt.Errorf("read owner %s: %w", cfg.Owner, err)
	}
	a := &app{cfg: cfg, gh: gh, col: col, org: org, stateDir: stateDir}
	a.providers, a.routes = build(cfg, includedMinutes(gh, cfg.Owner, org))
	return a, nil
}

// githubToken prefers GITHUB_TOKEN and falls back to the GitHub CLI's login.
func githubToken() (string, error) {
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t, nil
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return "", errors.New("no GitHub access: run gh auth login, or set GITHUB_TOKEN")
	}
	return strings.TrimSpace(string(out)), nil
}

// Labels zerobill writes for the hosted tiers.
const (
	githubLabel     = "ubuntu-latest"
	blacksmithLabel = "blacksmith-2vcpu-ubuntu-2404"
	blacksmithFree  = 3000
)

// build turns the config into providers and the two routes: CI_RUNNER tries the
// user's runner and then the free tiers; BOT_RUNNER only the free tiers. Both
// fall back to the runner when one is configured, so jobs wait instead of
// costing money.
func build(cfg *config.Config, githubMinutes float64) ([]policy.Provider, []policy.Route) {
	var hosted []policy.Provider
	if cfg.Blacksmith {
		hosted = append(hosted, policy.Provider{Name: "blacksmith", Label: blacksmithLabel, Kind: policy.Metered, Monthly: blacksmithFree})
	}
	github := policy.Provider{Name: "github", Label: githubLabel, Kind: policy.Metered, Monthly: githubMinutes}
	hosted = append(hosted, github)
	ci, fallback := hosted, github
	if cfg.Runner != "" {
		runner := policy.Provider{Name: "runner", Label: cfg.Runner, Kind: policy.SelfHosted}
		ci, fallback = append([]policy.Provider{runner}, hosted...), runner
	}
	return ci, []policy.Route{
		{Variable: "CI_RUNNER", Providers: ci, Fallback: fallback},
		{Variable: "BOT_RUNNER", Providers: hosted, Fallback: fallback},
	}
}

// includedMinutes returns the plan's included Linux minutes: GitHub Free $12,
// Pro and Team $18, at $0.006 a minute. Unreadable plans assume Free.
func includedMinutes(gh *github.Client, owner string, org bool) float64 {
	var acct struct {
		Plan struct {
			Name string `json:"name"`
		} `json:"plan"`
	}
	path := "/user"
	if org {
		path = "/orgs/" + owner
	}
	if gh.Get(path, &acct) == nil {
		switch strings.ToLower(acct.Plan.Name) {
		case "pro", "team":
			return 3000
		}
	}
	return 2000
}

func (a *app) pausePath() string   { return filepath.Join(a.stateDir, "paused.json") }
func (a *app) appliedPath() string { return filepath.Join(a.stateDir, "applied.json") }

func providerArg(a *app, args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("name a provider: runner, blacksmith or github")
	}
	for _, p := range a.providers {
		if p.Name == args[0] {
			return p.Name, nil
		}
	}
	return "", fmt.Errorf("%s is not configured; providers are runner (with runner: set), blacksmith (with blacksmith: true) and github", args[0])
}

func pause(args []string, now time.Time) error {
	a, err := loadLocal()
	if err != nil {
		return err
	}
	name, err := providerArg(a, args)
	if err != nil {
		return err
	}
	e := override.Entry{Since: now}
	if len(args) > 1 {
		d, err := parseDuration(args[1])
		if err != nil {
			return err
		}
		e.Until = now.Add(d)
	}
	set, err := override.Load(a.pausePath())
	if err != nil {
		return err
	}
	if set.Paused == nil {
		set.Paused = map[string]override.Entry{}
	}
	set.Paused[name] = e
	if err := override.Save(a.pausePath(), set); err != nil {
		return err
	}
	fmt.Printf("%s: %s\n", name, set.Active(now)[name])
	return nil
}

func resume(args []string) error {
	a, err := loadLocal()
	if err != nil {
		return err
	}
	name, err := providerArg(a, args)
	if err != nil {
		return err
	}
	set, err := override.Load(a.pausePath())
	if err != nil {
		return err
	}
	delete(set.Paused, name)
	if err := override.Save(a.pausePath(), set); err != nil {
		return err
	}
	fmt.Printf("%s: resumed\n", name)
	return nil
}

// loadLocal reads the config without contacting GitHub, for pause and resume.
func loadLocal() (*app, error) {
	path, err := config.Path()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	stateDir, err := config.StateDir()
	if err != nil {
		return nil, err
	}
	a := &app{cfg: cfg, stateDir: stateDir}
	a.providers, a.routes = build(cfg, 0)
	return a, nil
}

// parseDuration accepts Go durations and whole days such as 7d.
func parseDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("duration %q: use for example 72h or 7d", s)
	}
	return d, nil
}

// flags parses args with fs, accepting flags only.
func flags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return nil
}
