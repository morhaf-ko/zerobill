package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/morhaf-ko/zerobill/internal/discover"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

func status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	if err := flags(fs, args); err != nil {
		return err
	}
	a, err := load()
	if err != nil {
		return err
	}
	c, err := newController(a, false)
	if err != nil {
		return err
	}
	found, err := discover.Discover(a.gh, a.cfg.Owner, a.org)
	if err != nil {
		return fmt.Errorf("list private repositories: %w", err)
	}
	c.repos = found.Repos
	a.col.Repositories = names(found.Repos)
	now := time.Now().UTC()
	r := a.col.Observe(a.providers)
	errs := map[string]string{}
	for k, v := range r.Errors {
		errs[k] = v
	}
	states := c.adjust(now, r.States, errs)
	for k, v := range r.Warnings {
		errs[k] = v
	}
	return render(os.Stdout, now, a, c.state.Routes, states, found, errs)
}

// render prints each provider's budget, each variable's choice, the savings
// estimate and anything that stops zerobill from routing.
func render(w io.Writer, now time.Time, a *app, applied map[string]policy.Applied, states map[string]policy.State, found discover.Result, errs map[string]string) error {
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
			fmt.Fprintf(tw, "%s\t%s\t%s\t%.0f min\t-\t-\n", p.Name, p.Label, why, s.UsedMinutes)
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
	if saved := policy.Saved(a.providers, states); saved > 0 {
		fmt.Fprintf(w, "Saved this month: about $%.2f, your runner's minutes at GitHub's price.\n", saved)
	}

	var problems []string
	keys := make([]string, 0, len(errs))
	for k := range errs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		problems = append(problems, k+": "+errs[k])
	}
	if errs["runner"] != "" || errs["github"] != "" {
		scope := "user"
		if a.org {
			scope = "admin:org"
		}
		problems = append(problems, "reading runners or billing needs the "+scope+" scope: run gh auth refresh -s "+scope+", or give GITHUB_TOKEN that access")
	}
	if len(found.Repos) == 0 {
		problems = append(problems, "no private repository reads vars.CI_RUNNER yet; set runs-on: ${{ vars.CI_RUNNER }} in your workflows")
	}
	for _, f := range found.Fixed {
		problems = append(problems, "fixed runner, change to ${{ vars.CI_RUNNER }}: "+f)
	}
	for _, e := range found.Errors {
		problems = append(problems, "could not read workflows: "+e)
	}
	if len(problems) > 0 {
		fmt.Fprintf(w, "\nProblems:\n  %s\n", strings.Join(problems, "\n  "))
	}
	return nil
}
