// Command zerobill keeps GitHub Actions jobs on free runners.
package main

import (
	"errors"
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
	pauses "github.com/morhaf-ko/zerobill/internal/pause"
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
  zerobill init                         create a config with guided prompts
  zerobill [status]                     free minutes, current choices and problems
  zerobill run [--apply] [--health :8080]
                                        keep CI_RUNNER and BOT_RUNNER on the best free
                                        runner, every 5 minutes; without --apply it
                                        only logs; --health serves /healthz
  zerobill pause runner|blacksmith|github [duration]
  zerobill resume runner|blacksmith|github
  zerobill version

Config: ~/.config/zerobill/config.yaml with owner, and optionally runner, blacksmith,
reserve_percent (default 10) and notify; or the matching ZEROBILL_* environment variables.
GitHub access: GITHUB_TOKEN, else the GitHub CLI login (gh auth token).`

func main() {
	cmd, args := "status", os.Args[1:]
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "init":
		err = initConfig(args, os.Stdin, os.Stdout)
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

// app is what every command needs: the config and the providers and routes it
// implies, plus GitHub access for commands that read GitHub.
type app struct {
	cfg       *config.Config
	stateDir  string
	providers []policy.Provider
	routes    []policy.Route
	gh        *github.Client
	col       *collect.Collector
	org       bool
}

// load reads the config; with online set it also connects to GitHub.
func load(online bool) (*app, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	stateDir, err := config.StateDir()
	if err != nil {
		return nil, err
	}
	a := &app{cfg: cfg, stateDir: stateDir}
	if !online {
		a.providers, a.routes = build(cfg, 0)
		return a, nil
	}
	token, err := githubToken()
	if err != nil {
		return nil, err
	}
	a.gh = github.New(token)
	if a.org, err = collect.IsOrg(a.gh, cfg.Owner); err != nil {
		return nil, fmt.Errorf("read owner %s: %w", cfg.Owner, err)
	}
	var acct struct {
		Login string `json:"login"`
		Plan  struct {
			Name string `json:"name"`
		} `json:"plan"`
	}
	if a.org {
		// The plan is best effort: only organization owners can read it.
		_ = a.gh.Get("/orgs/"+cfg.Owner, &acct)
	} else {
		if err := a.gh.Get("/user", &acct); err != nil {
			return nil, fmt.Errorf("read the signed-in user: %w", err)
		}
		if !strings.EqualFold(acct.Login, cfg.Owner) {
			return nil, fmt.Errorf("owner %s is a personal account, so zerobill must run as that user, not %s", cfg.Owner, acct.Login)
		}
	}
	a.col = &collect.Collector{GH: a.gh, Owner: cfg.Owner, Org: a.org, StatePath: filepath.Join(stateDir, "cache.json")}
	a.providers, a.routes = build(cfg, includedMinutes(acct.Plan.Name))
	return a, nil
}

// loadConfig prefers ZEROBILL_* environment variables and falls back to the file.
func loadConfig() (*config.Config, error) {
	if cfg, ok, err := config.FromEnv(); ok {
		return cfg, err
	}
	path, err := config.Path()
	if err != nil {
		return nil, err
	}
	return config.Load(path)
}

// githubToken prefers GITHUB_TOKEN and falls back to the GitHub CLI's login.
func githubToken() (string, error) {
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t, nil
	}
	out, err := exec.Command("gh", "auth", "token", "--hostname", "github.com").Output()
	if errors.Is(err, exec.ErrNotFound) {
		return "", errors.New("no GitHub access: set GITHUB_TOKEN, or install the GitHub CLI and run gh auth login")
	}
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
		hosted = append(hosted, policy.Provider{Name: "blacksmith", Label: blacksmithLabel, Kind: policy.Metered, Monthly: blacksmithFree, ReservePercent: int(cfg.ReservePercent)})
	}
	github := policy.Provider{Name: "github", Label: githubLabel, Kind: policy.Metered, Monthly: githubMinutes, ReservePercent: int(cfg.ReservePercent)}
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

// includedMinutes returns a plan's included Linux minutes: GitHub Free $12, Pro
// and Team $18, at $0.006 a minute. Unknown plans assume Free.
func includedMinutes(plan string) float64 {
	switch strings.ToLower(plan) {
	case "pro", "team":
		return 3000
	default:
		return 2000
	}
}

func (a *app) pausePath() string   { return filepath.Join(a.stateDir, "paused.json") }
func (a *app) appliedPath() string { return filepath.Join(a.stateDir, "applied.json") }

// providerArg returns the provider args[0] names, if the config has it.
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
	a, err := load(false)
	if err != nil {
		return err
	}
	name, err := providerArg(a, args)
	if err != nil {
		return err
	}
	var until time.Time
	if len(args) > 1 {
		d, err := parseDuration(args[1])
		if err != nil {
			return err
		}
		until = now.Add(d)
	}
	set, err := pauses.Load(a.pausePath())
	if err != nil {
		return err
	}
	set[name] = until
	if err := set.Save(a.pausePath()); err != nil {
		return err
	}
	fmt.Printf("%s: %s\n", name, set.Active(now)[name])
	return nil
}

func resume(args []string) error {
	a, err := load(false)
	if err != nil {
		return err
	}
	name, err := providerArg(a, args)
	if err != nil {
		return err
	}
	set, err := pauses.Load(a.pausePath())
	if err != nil {
		return err
	}
	delete(set, name)
	if err := set.Save(a.pausePath()); err != nil {
		return err
	}
	fmt.Printf("%s: resumed\n", name)
	return nil
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
