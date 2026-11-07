package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/morhaf-ko/zerobill/internal/apply"
	"github.com/morhaf-ko/zerobill/internal/config"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

func status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := fs.String("config", "zerobill.yaml", "routing policy")
	statePath := fs.String("state", "zerobill-state.json", "job cache")
	overridesPath := fs.String("overrides", "zerobill-overrides.json", "providers disabled with zerobill disable")
	appliedPath := fs.String("applied", "zerobill-applied.json", "watch's state, for on-demand stalls")
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
	ctl := &controller{cfg: cfg, providers: providers, routes: cfg.PolicyRoutes(providers), overridesPath: *overridesPath}
	if ctl.state, err = apply.LoadState(*appliedPath); err != nil {
		return err
	}
	errs := map[string]string{}
	for k, v := range r.Errors {
		errs[k] = v
	}
	states := ctl.adjust(time.Now().UTC(), r.States, errs)
	return renderStatus(os.Stdout, time.Now().UTC(), cfg, providers, ctl.routes, states, errs, r.Warnings)
}

// renderStatus prints each provider's budget and month-end pace, each route's
// current choice and the savings estimate.
func renderStatus(w io.Writer, now time.Time, cfg *config.Config, providers []policy.Provider, routes []policy.Route, states map[string]policy.State, errs, warnings map[string]string) error {
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	elapsed := now.Sub(start)
	share := elapsed.Seconds() / end.Sub(start).Seconds()
	fmt.Fprintf(w, "%s · %s · %.0f%% of the month elapsed · mode %s\n\n", cfg.Owner, now.Format("2 Jan 2006 15:04 UTC"), share*100, cfg.Mode)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tKIND\tSTATE\tUSED\tLIMIT\tLEFT\tMONTH-END AT THIS PACE")
	for _, p := range providers {
		s, ok := states[p.Name]
		if !ok {
			fmt.Fprintf(tw, "%s\t%s\tnot observed, see below\t\t\t\t\n", p.Name, p.Kind)
			continue
		}
		_, why, _ := policy.Usable(p, s)
		if p.Kind == policy.SelfHosted {
			used := "-"
			if s.UsedMinutes > 0 {
				used = fmt.Sprintf("%.0f", s.UsedMinutes)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t-\t-\t-\n", p.Name, p.Kind, why, used)
			continue
		}
		limit := p.MonthlyMinutes - p.ReserveMinutes
		pace := "-"
		if s.UsedMinutes > 0 && share > 0 {
			projected := s.UsedMinutes / share
			pace = fmt.Sprintf("%.0f", projected)
			if projected > limit && s.UsedMinutes < limit {
				at := start.Add(time.Duration(float64(elapsed) * limit / s.UsedMinutes))
				pace += fmt.Sprintf(", reserve on %s", at.Format("2 Jan"))
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.0f\t%.0f\t%.0f\t%s\n", p.Name, p.Kind, short(why, 40), s.UsedMinutes, limit, max(limit-s.UsedMinutes, 0), pace)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROUTE\tVARIABLES\tCHOICE\tREASON")
	for _, rt := range routes {
		d, err := policy.Decide(rt, states)
		if err != nil {
			fmt.Fprintf(tw, "%s\t%v\terror\t%s\n", rt.Name, rt.Variables, err)
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s (%s)\t%s\n", rt.Name, joinOrDash(rt.Variables), d.Provider, d.Label, short(d.Reason, 90))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	saved := policy.Saved(providers, states)
	fmt.Fprintf(w, "\nSaved this month: about $%.2f, self-hosted minutes priced at GitHub's $%.3f a minute.\n", saved, policy.GitHubMinuteUSD)
	fmt.Fprintln(w, "watch also applies min_hold, so its choice can lag this view.")
	for _, m := range []map[string]string{errs, warnings} {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "%s: %s\n", k, m[k])
		}
	}
	return nil
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func joinOrDash(v []string) string {
	if len(v) == 0 {
		return "-"
	}
	out := v[0]
	for _, s := range v[1:] {
		out += "," + s
	}
	return out
}
