package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/morhaf-ko/zerobill/internal/apply"
	"github.com/morhaf-ko/zerobill/internal/atomicfile"
	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/config"
	"github.com/morhaf-ko/zerobill/internal/discover"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

var errStatusIncomplete = errors.New("status incomplete")

func status(args []string) error {
	if len(args) == 0 {
		a, err := load(false, nil)
		if err != nil {
			return err
		}
		return localStatus(os.Stdout, a, time.Now().UTC())
	}
	if len(args) != 1 || args[0] != "--refresh" {
		return errors.New("usage: zerobill status [--refresh]")
	}
	progress := statusProgress(os.Stderr)
	progress("")
	a, err := load(true, progress)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "GitHub: %s via %s; owner: %s.\n", a.login, a.authSource, a.cfg.Owner)
	return reportStatus(os.Stdout, a)
}

type statusSnapshot struct {
	Owner           string                    `json:"owner"`
	Checked         time.Time                 `json:"checked"`
	SelfHostedLabel string                    `json:"self_hosted_label,omitempty"`
	GitHubMinutes   float64                   `json:"github_minutes"`
	States          map[string]policy.State   `json:"states"`
	Applied         map[string]policy.Applied `json:"applied,omitempty"`
	UsesCI          bool                      `json:"uses_ci,omitempty"`
	UsesBot         bool                      `json:"uses_bot,omitempty"`
	DiscoveryFailed bool                      `json:"discovery_failed,omitempty"`
	Incomplete      bool                      `json:"incomplete,omitempty"`
}

func (a *app) statusPath() string  { return filepath.Join(a.stateDir, "status.json") }
func (a *app) refreshPath() string { return filepath.Join(a.stateDir, "status-refresh.json") }

func (a *app) snapshot(checked time.Time, repos []discover.Repo, states map[string]policy.State) statusSnapshot {
	s := statusSnapshot{Owner: a.cfg.Owner, Checked: checked, SelfHostedLabel: a.cfg.SelfHostedLabel, States: states}
	for _, p := range a.providers {
		if p.Name == "github" {
			s.GitHubMinutes = p.Monthly
		}
	}
	for _, repo := range repos {
		s.UsesCI = s.UsesCI || reads(repo, "CI_RUNNER")
		s.UsesBot = s.UsesBot || reads(repo, "BOT_RUNNER")
	}
	return s
}

func saveStatus(path string, s statusSnapshot) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, raw)
}

func (c *controller) saveStatus(checked time.Time, states map[string]policy.State, incomplete bool) error {
	s := c.a.snapshot(checked, c.repos, states)
	s.DiscoveryFailed = c.discoverErr != "" || len(c.workflowErrors) > 0
	s.Incomplete = incomplete || s.DiscoveryFailed
	st, err := apply.LoadState(c.a.appliedPath())
	if err != nil {
		return err
	}
	for variable, route := range st.Routes {
		if strings.EqualFold(route.Owner, s.Owner) {
			if s.Applied == nil {
				s.Applied = make(map[string]policy.Applied)
			}
			s.Applied[variable] = route
		}
	}
	return saveStatus(c.a.statusPath(), s)
}

func loadStatus(path, owner string) (statusSnapshot, error) {
	var s statusSnapshot
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return s, err
	default:
		if err := json.Unmarshal(raw, &s); err != nil {
			return statusSnapshot{}, fmt.Errorf("read saved status: %w", err)
		}
	}
	if !strings.EqualFold(s.Owner, owner) {
		return statusSnapshot{}, nil
	}
	return s, nil
}

func localStatus(w io.Writer, a *app, now time.Time) error {
	s, controllerErr := loadStatus(a.statusPath(), a.cfg.Owner)
	refresh, refreshErr := loadStatus(a.refreshPath(), a.cfg.Owner)
	coverageChecked := s.Checked
	switch {
	case controllerErr != nil:
		if refresh.Checked.IsZero() || refreshErr != nil {
			return controllerErr
		}
		fmt.Fprintln(w, "Controller assignments unavailable. Check the controller logs.")
		s = refresh
	case s.Checked.IsZero():
		if refreshErr != nil {
			return refreshErr
		}
		s = refresh
		coverageChecked = refresh.Checked
	case refresh.Checked.After(s.Checked) && refreshErr == nil:
		s.Checked, s.States = refresh.Checked, refresh.States
		s.GitHubMinutes, s.SelfHostedLabel = refresh.GitHubMinutes, refresh.SelfHostedLabel
	}
	if s.Checked.IsZero() {
		fmt.Fprintf(w, "%s\n\n", a.cfg.Owner)
		if err := renderCapacity(w, a.providers, nil, false, time.Time{}); err != nil {
			return err
		}
		fmt.Fprintln(w, "\nNo saved check for this owner. Run zerobill status --refresh.")
		return nil
	}
	providers, routes := build(a.cfg, s.GitHubMinutes)
	if s.SelfHostedLabel != a.cfg.SelfHostedLabel {
		delete(s.States, "runner")
	}
	currentMonth := s.Checked.UTC().Format("2006-01") == now.Format("2006-01")
	age := max(now.Sub(s.Checked).Round(time.Second), 0)
	fresh := currentMonth && age <= 3*a.cfg.CheckInterval
	complete := !s.Incomplete
	for _, p := range providers {
		_, ok := s.States[p.Name]
		complete = complete && ok
	}
	fmt.Fprintf(w, "%s · Usage checked %s ago\n\n", a.cfg.Owner, age)
	if err := renderCapacity(w, providers, s.States, currentMonth, time.Time{}); err != nil {
		return err
	}
	fmt.Fprintln(w)
	if s.UsesCI || s.UsesBot {
		fmt.Fprintln(w, "Routing")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "VARIABLE\tLAST APPLIED\tRECOMMENDED\tREASON")
		for _, rt := range routes {
			if rt.Variable == "CI_RUNNER" && !s.UsesCI || rt.Variable == "BOT_RUNNER" && !s.UsesBot {
				continue
			}
			applied := s.Applied[rt.Variable]
			last := applied.Label
			if last == "" {
				last = "No assignment"
			}
			next, reason := "-", "-"
			if fresh && complete && now.Sub(coverageChecked) <= 3*a.cfg.CheckInterval {
				d, _ := policy.Hold(policy.Decide(rt, s.States), applied, rt, s.States, s.Checked)
				if d.Label != applied.Label {
					next, reason = d.Label, d.Reason
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", rt.Variable, last, next, reason)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}
	if s.DiscoveryFailed {
		fmt.Fprintln(w, "Workflow discovery was incomplete.")
	} else if !s.UsesCI && !s.UsesBot {
		fmt.Fprintln(w, "No connected workflows in the last check.")
	}
	if s.Incomplete {
		fmt.Fprintln(w, "The last controller check reported problems. See its logs.")
	}
	if !fresh || !complete || !s.UsesCI && !s.UsesBot {
		fmt.Fprintln(w, "Run zerobill status --refresh for live usage and setup checks.")
	}
	return nil
}

func reportStatus(w io.Writer, a *app) error {
	st, err := apply.LoadState(a.appliedPath())
	if err != nil {
		return err
	}
	found, err := discover.Discover(a.gh, a.cfg.Owner, a.org)
	if err != nil {
		return connectionProblem("read private repositories and workflows", a.authSource, a.cfg.Owner, a.org, err)
	}
	if len(found.Repos) == 0 || len(found.Errors) > 0 {
		return render(w, time.Now().UTC(), a, st.Routes, nil, found, collect.Result{AccessProblem: found.AccessProblem, RateLimited: found.RateLimited})
	}
	a.col.Repositories = names(found.Repos)
	now := time.Now().UTC()
	r := statusObservations(a, os.Stderr)
	states := adjust(a, &st, now, r.States, r.Errors)
	if err := render(w, now, a, st.Routes, states, found, r); err != nil {
		return err
	}
	if err := saveStatus(a.refreshPath(), a.snapshot(now, found.Repos, states)); err != nil {
		return fmt.Errorf("save status: %w", err)
	}
	return nil
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

	if err := renderCapacity(w, a.providers, states, true, now); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nRouting")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VARIABLE\tRECOMMENDED\tREASON")
	for _, rt := range a.routes {
		used := false
		for _, repo := range found.Repos {
			used = used || reads(repo, rt.Variable)
		}
		if rt.Variable == "BOT_RUNNER" && !used {
			continue
		}
		d, _ := policy.Hold(policy.Decide(rt, states), applied[rt.Variable], rt, states, now)
		fmt.Fprintf(tw, "%s\t%s\t%s\n", rt.Variable, d.Label, d.Reason)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	variables := "CI_RUNNER"
	for _, repo := range found.Repos {
		if reads(repo, "BOT_RUNNER") {
			variables = "CI_RUNNER or BOT_RUNNER"
			break
		}
	}
	fmt.Fprintf(w, "\nRepositories: %d read %s.\n", len(found.Repos), variables)

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
		workflowSteps(w, found, a.authSource)
	}
	fmt.Fprintln(w, "\nFree minutes exclude each provider’s reserve.")
	if a.cfg.Blacksmith {
		fmt.Fprintln(w, "Blacksmith usage is estimated from GitHub Actions job history; no Blacksmith API key is used.")
	}
	checkInterval := a.cfg.CheckInterval
	if checkInterval == 0 {
		checkInterval = config.DefaultCheckInterval
	}
	queueTimeout := a.cfg.QueueTimeout
	if queueTimeout == 0 {
		queueTimeout = config.DefaultQueueTimeout
	}
	fmt.Fprintf(w, "Checks: every %s; self-hosted queue timeout: %s.\n", checkInterval, queueTimeout)
	for _, alert := range (&controller{a: a}).capacityAlerts(now, states) {
		fmt.Fprintln(w, alert.Message)
	}
	if a.cfg.Notify == "" {
		fmt.Fprintln(w, "Alerts: not configured; set notify in your config to receive routing and low-capacity alerts.")
	} else if a.cfg.AlertRemainingPercent == 0 {
		fmt.Fprintln(w, "Low-capacity alerts: disabled (alert_remaining_percent: 0).")
	} else {
		fmt.Fprintf(w, "Low-capacity alerts: %d%% remaining, once per provider per UTC month during run --apply.\n", a.cfg.AlertRemainingPercent)
	}
	for _, p := range a.providers {
		if p.Kind == policy.Metered && a.cfg.AlertRemainingPercent > 0 && a.cfg.AlertRemainingPercent <= p.ReservePercent {
			fmt.Fprintln(w, "For advance warning, set alert_remaining_percent above the reserves of enabled providers.")
			break
		}
	}
	fmt.Fprintln(w, "Spending protection: provider limits are not verified. Configure billing limits separately; routing cannot guarantee zero charges.")
	fmt.Fprintln(w, "\nNext: preview decisions without changing GitHub:\n  zerobill run\nAfter reviewing those decisions, enable routing with:\n  zerobill run --apply")
	return nil
}

// A zero projection time hides the month-end estimate for saved status.
func renderCapacity(w io.Writer, providers []policy.Provider, states map[string]policy.State, currentMonth bool, projectionTime time.Time) error {
	fmt.Fprintln(w, "Capacity")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	columns := "PROVIDER\tLABEL\tSTATE\tUSED\tRESERVE\tFREE"
	if !projectionTime.IsZero() {
		columns += "\tMONTH END AT THIS PACE"
	}
	fmt.Fprintln(tw, columns)
	names := map[string]string{"runner": "Self-hosted", "blacksmith": "Blacksmith", "github": "GitHub Actions"}
	for _, p := range providers {
		s, ok := states[p.Name]
		state, used, reserve, free, pace := "Unavailable", "-", "-", "-", "-"
		if p.Kind == policy.Metered {
			reserve = fmt.Sprintf("%d%%", p.ReservePercent)
		}
		switch {
		case ok && p.Kind == policy.SelfHosted:
			_, state = policy.Usable(p, s)
		case ok && currentMonth:
			usable, why := policy.Usable(p, s)
			state = why
			if usable {
				state = "Available"
			}
			used = fmt.Sprintf("%.0f min", s.UsedMinutes)
			free = fmt.Sprintf("%.0f min", max(p.Limit()-s.UsedMinutes, 0))
			if !projectionTime.IsZero() {
				pace = projectedUsage(p, s, projectionTime)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s", names[p.Name], p.Label, state, used, reserve, free)
		if !projectionTime.IsZero() {
			fmt.Fprintf(tw, "\t%s", pace)
		}
		fmt.Fprintln(tw)
	}
	return tw.Flush()
}

func projectedUsage(p policy.Provider, s policy.State, now time.Time) string {
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	elapsed := now.Sub(start)
	if s.UsedMinutes <= 0 || elapsed <= 0 {
		return "-"
	}
	projected := s.UsedMinutes * start.AddDate(0, 1, 0).Sub(start).Seconds() / elapsed.Seconds()
	pace := fmt.Sprintf("%.0f min", projected)
	if projected > p.Limit() && s.UsedMinutes < p.Limit() {
		pace += ", runs out " + start.Add(time.Duration(float64(elapsed)*p.Limit()/s.UsedMinutes)).Format("2 Jan")
	}
	return pace
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
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CHECK\tRESULT")
	for _, key := range keys {
		fmt.Fprintf(tw, "%s\tunavailable (read failed)\n", key)
	}
	if len(found.Errors) > 0 {
		fmt.Fprintln(tw, "Workflows\tsome could not be read")
	}
	if found.PrivateRepos == 0 && len(found.Repos) == 0 {
		fmt.Fprintln(tw, "Private repositories\tnone visible. The owner may have none, or this credential may lack access.")
	} else if len(found.Repos) == 0 && len(found.Errors) == 0 {
		fmt.Fprintf(tw, "Private repositories\t%d visible; no workflows use CI_RUNNER or BOT_RUNNER yet.\n", found.PrivateRepos)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage estimates and runner recommendations are withheld until setup and reads succeed.")
	fmt.Fprintln(w, "\nNext:")
	switch {
	case result.RateLimited:
		fmt.Fprintln(w, "GitHub is rate limiting requests. Wait until its rate limit resets (at least a minute for secondary limits), then run:\n  zerobill status --refresh")
	case result.AccessProblem:
		fmt.Fprintln(w, accessSteps(a.authSource, a.org))
	case found.PrivateRepos == 0 && len(found.Repos) == 0:
		if a.authSource == "GITHUB_TOKEN" || a.authSource == "GH_TOKEN" {
			fmt.Fprintln(w, accessSteps(a.authSource, a.org))
		} else {
			fmt.Fprintf(w, "Check the configured owner and private repository visibility:\n  %s repo list %s --visibility private\n", ghCommand(a.authSource), shellQuote(a.cfg.Owner))
			fmt.Fprintf(w, "If repositories are missing, check access with:\n  %s auth status --hostname github.com\n", ghCommand(a.authSource))
		}
	case len(found.Errors) > 0:
		fmt.Fprintln(w, "Check the workflow errors below, then run:\n  zerobill status --refresh")
	case errs["pause"] != "":
		fmt.Fprintf(w, "Check the local pause state:\n  ls -ld -- %s\n", shellQuote(a.pausePath()))
		fmt.Fprintln(w, "It must be a readable file, not a directory. Fix it before retrying:\n  zerobill status --refresh")
	case len(errs) > 0:
		if errs["github"] != "" {
			if a.authSource == "GITHUB_TOKEN" || a.authSource == "GH_TOKEN" {
				fmt.Fprintln(w, accessSteps(a.authSource, a.org))
			}
			fmt.Fprintf(w, "Check billing access as the account owner or organization administrator:\n  %s\n", billingURL(a.cfg.Owner, a.org))
			fmt.Fprintln(w, "The billing API may be unavailable for this account; refreshing scopes alone may not fix it.")
		}
		fmt.Fprintln(w, "Check the API errors below and retry when the service is available:\n  zerobill status --refresh")
	default:
		workflowSteps(w, found, a.authSource)
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

func workflowSteps(w io.Writer, found discover.Result, source string) {
	repo := found.ExampleRepo
	if len(found.Fixed) > 0 {
		repo = strings.Fields(found.Fixed[0])[0]
	}
	if repo != "" {
		fmt.Fprintf(w, "For example, to route %s, first create the variable:\n  %s variable set CI_RUNNER --repo %s --body zerobill-wait\n", repo, ghCommand(source), shellQuote(repo))
	} else {
		fmt.Fprintln(w, "In each private repository you want to route, create CI_RUNNER with an initial value of zerobill-wait.")
	}
	fmt.Fprintln(w, "Then set the workflow's runner to:\n  runs-on: ${{ vars.CI_RUNNER }}\nCheck again:\n  zerobill status --refresh\nzerobill-wait queues jobs without selecting paid capacity; never assign it to a runner.\nAfter run --apply selects capacity, cancel and rerun jobs already queued on zerobill-wait.")
}

// Check inexpensive reads before starting the cold monthly job scan.
func statusObservations(a *app, progress io.Writer) collect.Result {
	r := collect.Result{States: map[string]policy.State{}, Errors: map[string]string{}}
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
