package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/github"
	"github.com/morhaf-ko/zerobill/internal/setup"
)

// labelName matches a runner label, leaving out YAML fragments and expressions.
var labelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func initConfig(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	owner := fs.String("owner", "", "organization or user to route (required)")
	out := fs.String("out", "zerobill.yaml", "config file to write")
	force := fs.Bool("force", false, "overwrite an existing config")
	_ = fs.Parse(args)
	if *owner == "" {
		return errors.New("--owner is required")
	}
	if _, err := os.Stat(*out); err == nil && !*force {
		return fmt.Errorf("%s exists; pass --force to overwrite it", *out)
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return errors.New("GITHUB_TOKEN is not set")
	}
	d, notes, err := detect(github.New(token), *owner)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte(d.Render(time.Now().UTC())), 0o644); err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Println(n)
	}
	fmt.Printf("wrote %s for %d repositories\n", *out, len(d.Repos))
	compose := filepath.Join(filepath.Dir(*out), "compose.yaml")
	if _, err := os.Stat(compose); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(compose, []byte(setup.Compose), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", compose)
	} else {
		fmt.Printf("kept the existing %s\n", compose)
	}
	fmt.Printf("next: zerobill doctor --config %s, then docker compose up -d\n", *out)
	return nil
}

// detect reads the owner's private repositories, their runs-on declarations,
// variables and runners, and drafts a config from them.
func detect(gh *github.Client, owner string) (setup.Draft, []string, error) {
	d := setup.Draft{Owner: owner, GitHub: "ubuntu-latest", GitHubMinutes: 2000}
	col := &collect.Collector{GH: gh, Owner: owner}
	org, err := col.OwnerIsOrg()
	if err != nil {
		return d, nil, err
	}
	repos, err := setup.PrivateRepos(gh, owner, org)
	if err != nil {
		return d, nil, fmt.Errorf("list private repositories: %w", err)
	}
	var notes, labels, vars []string
	for _, repo := range repos {
		decls, err := setup.Workflows(gh, repo)
		if err != nil {
			notes = append(notes, fmt.Sprintf("skipped %s: cannot read workflows (%v)", repo, err))
			continue
		}
		if len(decls) == 0 {
			continue
		}
		d.Repos = append(d.Repos, repo)
		values, err := setup.Variables(gh, repo)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: cannot read variables (%v)", repo, err))
		}
		for _, decl := range decls {
			labels = append(labels, decl.Labels...)
			for _, v := range decl.Vars {
				vars = append(vars, v)
				if values[v] != "" {
					labels = append(labels, values[v])
				}
			}
		}
	}
	if len(d.Repos) == 0 {
		return d, nil, fmt.Errorf("no private repository of %s has workflows", owner)
	}
	slices.Sort(vars)
	vars = slices.Compact(vars)
	for _, v := range vars {
		if strings.Contains(v, "BOT") {
			d.BotVars = append(d.BotVars, v)
		} else {
			d.Vars = append(d.Vars, v)
		}
	}
	if len(d.Vars) == 0 {
		d.Vars = []string{"CI_RUNNER"}
		notes = append(notes, "no workflow reads a runner variable yet; zerobill doctor lists the runs-on lines to change")
	}

	runners, runnerErr := setup.Runners(gh, setup.RunnerPaths(owner, org, d.Repos))
	if err := runnerErr; err != nil {
		notes = append(notes, fmt.Sprintf("cannot read self-hosted runners (%v); add your runner's label by hand", err))
	}
	var registered []string
	managed := map[string]bool{}
	for _, r := range runners {
		registered = append(registered, r.Labels...)
		for _, l := range r.Labels {
			managed[l] = managed[l] || r.Managed
		}
	}
	selfHosted := func(l string) bool { return !setup.Hosted(l) && !setup.IsDefaultLabel(l) && labelName.MatchString(l) }
	d.SelfHosted = setup.MostCommon(labels, func(l string) bool { return selfHosted(l) && slices.Contains(registered, l) })
	if d.SelfHosted == "" {
		// Workflows use a label no runner carries now: a pool that creates runners
		// per job, unless the runners could not be read at all.
		d.SelfHosted = setup.MostCommon(labels, selfHosted)
		d.OnDemand = d.SelfHosted != "" && runnerErr == nil
	} else {
		d.OnDemand = managed[d.SelfHosted]
	}
	if d.SelfHosted == "" {
		d.SelfHosted = setup.MostCommon(registered, selfHosted)
	}
	d.Blacksmith = setup.MostCommon(labels, func(l string) bool { return strings.HasPrefix(l, "blacksmith-") })
	if l := setup.MostCommon(labels, func(l string) bool { return strings.HasPrefix(l, "ubuntu-") }); l != "" {
		d.GitHub = l
	}
	var known bool
	d.GitHubMinutes, known = includedMinutes(gh, owner, org)
	if !known {
		notes = append(notes, "could not read the GitHub plan; assumed GitHub Free's 2000 included minutes")
	}
	return d, notes, nil
}

// includedMinutes returns the plan's included Linux minutes, GitHub Free $12 and
// Pro and Team $18 at $0.006 a minute, and whether the plan was read.
func includedMinutes(gh *github.Client, owner string, org bool) (float64, bool) {
	var acct struct {
		Plan struct {
			Name string `json:"name"`
		} `json:"plan"`
	}
	path := "/user"
	if org {
		path = "/orgs/" + owner
	}
	if gh.Get(path, &acct) != nil {
		return 2000, false
	}
	switch strings.ToLower(acct.Plan.Name) {
	case "pro", "team":
		return 3000, true
	case "free":
		return 2000, true
	default:
		return 2000, false
	}
}
