package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/config"
	"github.com/morhaf-ko/zerobill/internal/github"
	"github.com/morhaf-ko/zerobill/internal/policy"
	"github.com/morhaf-ko/zerobill/internal/setup"
)

// checks prints one line per check and counts failures.
type checks struct {
	w                io.Writer
	ok, warned, fail int
}

func (c *checks) pass(format string, a ...any) {
	c.ok++
	fmt.Fprintf(c.w, "ok    %s\n", fmt.Sprintf(format, a...))
}

func (c *checks) warn(format string, a ...any) {
	c.warned++
	fmt.Fprintf(c.w, "warn  %s\n", fmt.Sprintf(format, a...))
}

func (c *checks) failf(format string, a ...any) {
	c.fail++
	fmt.Fprintf(c.w, "FAIL  %s\n", fmt.Sprintf(format, a...))
}

var errChecksFailed = errors.New("doctor found problems")

func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("config", "zerobill.yaml", "routing policy")
	_ = fs.Parse(args)
	c := &checks{w: os.Stdout}
	cfg, providers, err := loadConfig(*cfgPath)
	if err != nil {
		c.failf("config: %v", err)
		return errChecksFailed
	}
	c.pass("config %s: %d providers, %d routes, mode %s", *cfgPath, len(providers), len(cfg.Routes), cfg.Mode)
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		c.failf("GITHUB_TOKEN is not set")
		return errChecksFailed
	}
	runDoctor(c, github.New(token), cfg, providers, time.Now().UTC())
	fmt.Fprintf(c.w, "\n%d ok, %d warnings, %d failed\n", c.ok, c.warned, c.fail)
	if c.fail > 0 {
		return errChecksFailed
	}
	return nil
}

// runDoctor checks what watch needs with one read per permission and reports
// workflows whose runs-on no route controls. It writes nothing.
func runDoctor(c *checks, gh *github.Client, cfg *config.Config, providers []policy.Provider, now time.Time) {
	if cfg.Owner == "" {
		c.failf("config owner is not set")
		return
	}
	var me struct {
		Login string `json:"login"`
	}
	if err := gh.Get("/user", &me); err != nil {
		c.failf("token rejected: %v", err)
		return
	}
	col := &collect.Collector{GH: gh, Owner: cfg.Owner, Repositories: cfg.Repositories}
	org, err := col.OwnerIsOrg()
	if err != nil {
		c.failf("owner %s: %v", cfg.Owner, err)
		return
	}
	kind := "a user"
	if org {
		kind = "an organization"
	}
	c.pass("token user %s; owner %s is %s", me.Login, cfg.Owner, kind)
	if !org && !strings.EqualFold(me.Login, cfg.Owner) {
		c.failf("a user owner needs that user's token, got %s's", me.Login)
	}
	write := cfg.Mode == config.Switch
	permFail := false
	need := func(fine, classic string) string {
		permFail = true
		return fmt.Sprintf("the token needs %s (classic: %s)", fine, classic)
	}

	runners, err := setup.Runners(gh, setup.RunnerPaths(cfg.Owner, org, cfg.Repositories))
	switch {
	case err != nil && org:
		c.failf("read runners: %v; %s", err, need("Organization permission Self-hosted runners: read", "admin:org"))
	case err != nil:
		c.failf("read runners: %v; %s", err, need("Repository permission Administration: read", "repo"))
	default:
		c.pass("read %d self-hosted runner(s)", len(runners))
	}
	for _, p := range providers {
		if p.Kind != policy.SelfHosted || err != nil {
			continue
		}
		var registered, online int
		for _, r := range runners {
			if slices.Contains(r.All, p.Label) {
				registered++
				if r.Online {
					online++
				}
			}
		}
		switch {
		case registered > 0:
			c.pass("provider %s: %d runner(s) with label %s, %d online", p.Name, registered, p.Label, online)
		case p.OnDemand:
			c.pass("provider %s: no runner registered with %s; on_demand expects runners per job", p.Name, p.Label)
		default:
			c.warn("provider %s: no runner has label %s, so it counts as offline; set on_demand: true if runners are created per job", p.Name, p.Label)
		}
	}

	for _, p := range providers {
		switch {
		case p.Kind == policy.Metered && strings.HasPrefix(p.Label, "ubuntu-"):
			base, fine, classic := "/users/", "Account permission Plan: read", "user"
			if org {
				base, fine, classic = "/organizations/", "Organization permission Administration: read", "admin:org"
			}
			path := fmt.Sprintf("%s%s/settings/billing/usage?year=%d&month=%d", base, url.PathEscape(cfg.Owner), now.Year(), int(now.Month()))
			var report struct{}
			if err := gh.Get(path, &report); err != nil {
				c.failf("provider %s: read GitHub billing: %v; %s", p.Name, err, need(fine, classic))
			} else {
				c.pass("provider %s: read GitHub billing usage", p.Name)
			}
		case p.Kind == policy.SelfHosted || strings.HasPrefix(p.Label, "blacksmith-"):
			repos, err := col.OwnerRepos()
			if err != nil {
				c.failf("provider %s: list repositories: %v; %s", p.Name, err, need("Repository permission Metadata: read on all repositories", "repo"))
				continue
			}
			var denied []string
			for _, r := range repos {
				var runs struct{}
				if err := gh.Get("/repos/"+r+"/actions/runs?per_page=1", &runs); err != nil {
					denied = append(denied, r)
				}
			}
			switch {
			case len(denied) > 0 && p.Kind == policy.SelfHosted:
				c.warn("provider %s: cannot read runs in %s, so the savings estimate is unknown; %s", p.Name, strings.Join(denied, ", "), need("Repository permission Actions: read on all repositories", "repo"))
			case len(denied) > 0:
				c.failf("provider %s: cannot read runs in %s; Blacksmith minutes are summed over every repository, %s", p.Name, strings.Join(denied, ", "), need("Repository permission Actions: read on all repositories", "repo"))
			default:
				c.pass("provider %s: read runs in all %d repositories", p.Name, len(repos))
			}
		}
	}

	routed := map[string]string{}
	for _, rt := range cfg.Routes {
		for _, v := range rt.Variables {
			routed[v] = rt.Name
		}
	}
	for _, repo := range cfg.Repositories {
		var runs struct{}
		if err := gh.Get("/repos/"+repo+"/actions/runs?per_page=1", &runs); err != nil {
			c.failf("%s: read runs: %v; %s", repo, err, need("Repository permission Actions: read", "repo"))
			continue
		}
		decls, werr := setup.Workflows(gh, repo)
		vars, err := setup.Variables(gh, repo)
		if err != nil {
			c.failf("%s: read variables: %v; %s", repo, err, need("Repository permission Variables: read", "repo"))
		}
		if werr != nil {
			c.warn("%s: cannot read workflows (%v); add Repository permission Contents: read to check runs-on", repo, werr)
			continue
		}
		var used int
		read := map[string]bool{}
		for _, d := range decls {
			var reads []string
			for _, v := range d.Vars {
				if _, ok := routed[v]; ok {
					reads = append(reads, v)
					read[v] = true
				}
			}
			switch {
			case len(reads) > 0:
				used++
			case len(d.Vars) > 0:
				c.warn("%s %s:%d runs-on reads %s, which no route sets", repo, d.Path, d.Line, strings.Join(d.Vars, ", "))
			case strings.Contains(d.Value, "inputs."):
				// A reusable workflow's runner comes from its caller, whose file is checked.
				used++
			case strings.Contains(d.Value, "${{"):
				c.warn("%s %s:%d runs-on: %s is an expression without a route variable; zerobill cannot route it", repo, d.Path, d.Line, d.Value)
			default:
				c.warn("%s %s:%d runs-on: %s is fixed; use ${{ vars.%s }} so zerobill can route it", repo, d.Path, d.Line, d.Value, firstRoutedVar(cfg))
			}
		}
		if err == nil {
			var missing []string
			for v := range read {
				if _, ok := vars[v]; !ok {
					missing = append(missing, v)
				}
			}
			slices.Sort(missing)
			if len(missing) > 0 {
				c.warn("%s: workflows read %s, which is not a repository variable; unless an organization variable sets it, runs-on is empty until switch mode creates it", repo, strings.Join(missing, ", "))
			}
		}
		switch {
		case len(decls) > 0 && used == 0:
			c.warn("%s: none of %d jobs is routed", repo, len(decls))
		case len(decls) > 0:
			c.pass("%s: %d of %d jobs are routed", repo, used, len(decls))
		}
	}
	if write {
		c.warn("switch mode writes variables and cancels and reruns runs; doctor cannot check write access without writing, so the token needs Actions and Variables: write")
	}
	if permFail {
		fmt.Fprintf(c.w, "\nCreate a token with every permission zerobill needs:\n  %s\n", setup.TokenURL(cfg.Owner, org, write))
	}
}

func firstRoutedVar(cfg *config.Config) string {
	for _, rt := range cfg.Routes {
		if len(rt.Variables) > 0 {
			return rt.Variables[0]
		}
	}
	return "CI_RUNNER"
}
