package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/morhaf-ko/zerobill/internal/apply"
	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/discover"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

var errStatusIncomplete = errors.New("status incomplete")

func status(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unknown argument %q; status takes none", args[0])
	}
	a, err := load(true)
	if err != nil {
		return err
	}
	return reportStatus(os.Stdout, a)
}

func reportStatus(w io.Writer, a *app) error {
	st, err := apply.LoadState(a.appliedPath())
	if err != nil {
		return err
	}
	found, err := discover.Discover(a.gh, a.cfg.Owner, a.org)
	if err != nil {
		return connectionProblem("list private repositories", a.authSource, a.cfg.Owner, a.org, err)
	}
	if len(found.Repos) == 0 || len(found.Errors) > 0 {
		return render(w, time.Now().UTC(), a, st.Routes, nil, found, collect.Result{AccessProblem: found.AccessProblem, RateLimited: found.RateLimited})
	}
	a.col.Repositories = names(found.Repos)
	now := time.Now().UTC()
	r := statusObservations(a, os.Stderr)
	states := adjust(a, &st, now, r.States, r.Errors)
	return render(w, now, a, st.Routes, states, found, r)
}

// render prints each provider's budget, each variable's choice and anything that
// stops zerobill from routing.
func render(w io.Writer, now time.Time, a *app, applied map[string]policy.Applied, states map[string]policy.State, found discover.Result, result collect.Result) error {
	errs := result.Errors
	if len(errs) > 0 || len(found.Errors) > 0 || len(found.Repos) == 0 {
		return renderIncomplete(w, a, found, result)
	}
	fmt.Fprintf(w, "GitHub: %s via %s\n", a.login, a.authSource)
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	elapsed := now.Sub(start)
	share := elapsed.Seconds() / start.AddDate(0, 1, 0).Sub(start).Seconds()
	fmt.Fprintf(w, "%s, %s, %.0f%% of the month gone\n\n", a.cfg.Owner, now.Format("2 Jan 15:04 UTC"), share*100)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUNNER\tLABEL\tSTATE\tUSED\tFREE\tMONTH END AT THIS PACE")
	for _, p := range a.providers {
		s, ok := states[p.Name]
		if !ok {
			fmt.Fprintf(tw, "%s\t%s\tnot observed\t\t\t\n", p.Name, p.Label)
			continue
		}
		_, why := policy.Usable(p, s)
		if p.Kind == policy.SelfHosted {
			fmt.Fprintf(tw, "%s\t%s\t%s\t-\t-\t-\n", p.Name, p.Label, why)
			continue
		}
		pace := "-"
		if s.UsedMinutes > 0 && share > 0 {
			projected := s.UsedMinutes / share
			pace = fmt.Sprintf("%.0f min", projected)
			if projected > p.Limit() && s.UsedMinutes < p.Limit() {
				pace += ", runs out " + start.Add(time.Duration(float64(elapsed)*p.Limit()/s.UsedMinutes)).Format("2 Jan")
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.0f min\t%.0f min\t%s\n", p.Name, p.Label, why, s.UsedMinutes, max(p.Limit()-s.UsedMinutes, 0), pace)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VARIABLE\tUSE\tWHY")
	for _, rt := range a.routes {
		d, _ := policy.Hold(policy.Decide(rt, states), applied[rt.Variable], rt, states, now)
		fmt.Fprintf(tw, "%s\t%s\t%s\n", rt.Variable, d.Label, d.Reason)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "\nRepositories: %d read CI_RUNNER or BOT_RUNNER.\n", len(found.Repos))

	var problems []string
	for i, f := range found.Fixed {
		if i == maxFixed {
			problems = append(problems, fmt.Sprintf("and %d more jobs with a fixed runner", len(found.Fixed)-maxFixed))
			break
		}
		problems = append(problems, "fixed runner, change to ${{ vars.CI_RUNNER }}: "+f)
	}
	if len(problems) > 0 {
		fmt.Fprintf(w, "\nWorkflow setup:\n  %s\n", strings.Join(problems, "\n  "))
		workflowSteps(w, found)
	}
	fmt.Fprintf(w, "\nFREE excludes your %d%% reserve.\n", a.cfg.ReservePercent)
	if a.cfg.Blacksmith {
		fmt.Fprintln(w, "Blacksmith usage is estimated from GitHub Actions job history; no Blacksmith API key is used.")
	}
	fmt.Fprintln(w, "\nNext: preview decisions without changing GitHub:\n  zerobill run\nAfter reviewing those decisions, enable routing with:\n  zerobill run --apply")
	return nil
}

// maxFixed caps the fixed-runner lines, so a large organization's first status
// stays readable.
const maxFixed = 10

func renderIncomplete(w io.Writer, a *app, found discover.Result, result collect.Result) error {
	errs := result.Errors
	fmt.Fprintln(w, "Status incomplete: setup or access needs attention.")
	fmt.Fprintf(w, "Owner: %s\nGitHub: %s via %s\n\n", a.cfg.Owner, a.login, a.authSource)
	keys := make([]string, 0, len(errs))
	for key := range errs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(w, "%s: unavailable (read failed)\n", key)
	}
	if len(found.Errors) > 0 {
		fmt.Fprintln(w, "Workflows: some could not be read.")
	}
	if found.PrivateRepos == 0 && len(found.Repos) == 0 {
		fmt.Fprintln(w, "Private repositories: none visible. The owner may have none, or this credential may lack access.")
	} else if len(found.Repos) == 0 && len(found.Errors) == 0 {
		fmt.Fprintf(w, "Private repositories: %d visible; no workflows use CI_RUNNER or BOT_RUNNER yet.\n", found.PrivateRepos)
	}
	fmt.Fprintln(w, "Usage estimates and runner recommendations are withheld until setup and reads succeed.")
	fmt.Fprintln(w, "\nNext:")
	switch {
	case result.RateLimited:
		fmt.Fprintln(w, "GitHub is rate limiting requests. Wait until its rate limit resets (at least a minute for secondary limits), then run:\n  zerobill status")
	case result.AccessProblem:
		fmt.Fprintln(w, accessSteps(a.authSource, a.org))
	case found.PrivateRepos == 0 && len(found.Repos) == 0:
		if a.authSource == "GITHUB_TOKEN" || a.authSource == "GH_TOKEN" {
			fmt.Fprintln(w, accessSteps(a.authSource, a.org))
		} else {
			fmt.Fprintf(w, "Check the configured owner and private repository visibility:\n  gh repo list %s --visibility private\n", shellQuote(a.cfg.Owner))
			fmt.Fprintln(w, "If repositories are missing, check access with:\n  gh auth status --hostname github.com")
		}
	case len(found.Errors) > 0:
		fmt.Fprintln(w, "Check the workflow errors below, then run:\n  zerobill status")
	case len(errs) > 0:
		if errs["github"] != "" {
			if a.authSource == "GITHUB_TOKEN" || a.authSource == "GH_TOKEN" {
				fmt.Fprintln(w, accessSteps(a.authSource, a.org))
			}
			fmt.Fprintf(w, "Check billing access as the account owner or organization administrator:\n  %s\n", billingURL(a.cfg.Owner, a.org))
			fmt.Fprintln(w, "The billing API may be unavailable for this account; refreshing scopes alone may not fix it.")
		}
		fmt.Fprintln(w, "Check the API errors below and retry when the service is available:\n  zerobill status")
	default:
		workflowSteps(w, found)
	}
	if len(keys) > 0 || len(found.Errors) > 0 {
		fmt.Fprintln(w, "\nDetails:")
		for _, key := range keys {
			fmt.Fprintf(w, "  %s: %s\n", key, errs[key])
		}
		for _, err := range found.Errors {
			fmt.Fprintf(w, "  %s\n", err)
		}
	}
	return errStatusIncomplete
}

func workflowSteps(w io.Writer, found discover.Result) {
	if len(found.Fixed) > 0 {
		repo := strings.Fields(found.Fixed[0])[0]
		fmt.Fprintf(w, "For example, to route %s, first create the variable:\n  gh variable set CI_RUNNER --repo %s --body ubuntu-latest\n", repo, shellQuote(repo))
	} else {
		fmt.Fprintln(w, "In each private repository you want to route, create CI_RUNNER with an initial value of ubuntu-latest.")
	}
	fmt.Fprintln(w, "Then set the workflow's runner to:\n  runs-on: ${{ vars.CI_RUNNER }}\nCheck again:\n  zerobill status")
}

// Check inexpensive reads before starting the cold monthly job scan.
func statusObservations(a *app, progress io.Writer) collect.Result {
	r := collect.Result{States: map[string]policy.State{}}
	for _, name := range []string{"github", "runner", "blacksmith"} {
		for _, p := range a.providers {
			if p.Name != name {
				continue
			}
			if name == "blacksmith" {
				if _, err := os.Stat(a.col.StatePath); err != nil {
					fmt.Fprintln(progress, "Estimating Blacksmith minutes from GitHub Actions job history (no Blacksmith API key needed); the first scan can take a few minutes.")
				}
			}
			one := a.col.Observe([]policy.Provider{p})
			if len(one.Errors) > 0 {
				return one
			}
			for key, state := range one.States {
				r.States[key] = state
			}
		}
	}
	return r
}
