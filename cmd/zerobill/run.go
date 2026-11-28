package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/morhaf-ko/zerobill/internal/apply"
	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/config"
	"github.com/morhaf-ko/zerobill/internal/discover"
	"github.com/morhaf-ko/zerobill/internal/notify"
	pauses "github.com/morhaf-ko/zerobill/internal/pause"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

const (
	// rediscover is how often run rescans workflows; every interval would spend
	// the API rate limit on large organizations.
	rediscover = time.Hour
)

func run(args []string) error {
	write, healthAddr := false, ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--apply":
			write = true
		case args[i] == "--health":
			if i+1 == len(args) || args[i+1] == "" {
				return errors.New("--health needs an address, such as :8080")
			}
			i++
			healthAddr = args[i]
		default:
			return fmt.Errorf("unknown argument %q; run takes --apply and --health ADDRESS", args[i])
		}
	}
	var requests requestLog
	defer requests.flushOnSignal(os.Stderr)()
	a, err := load(&requests, nil)
	if err != nil {
		return err
	}
	c, err := newController(a, write)
	if err != nil {
		return err
	}
	beat := &heartbeat{staleAfter: max(3*a.cfg.CheckInterval, staleAfter)}
	beat.mark(time.Now())
	if healthAddr != "" {
		// Bind before the first check, so a bad address fails before anything is written.
		ln, err := net.Listen("tcp", healthAddr)
		if err != nil {
			return fmt.Errorf("--health: %w", err)
		}
		srv := &http.Server{Handler: beat, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			// A failed probe endpoint must not stop routing.
			fmt.Fprintln(os.Stderr, "zerobill: health:", srv.Serve(ln))
		}()
	}
	tick := time.NewTicker(a.cfg.CheckInterval)
	defer tick.Stop()
	var foundAt time.Time
	for {
		if time.Since(foundAt) >= rediscover {
			found, err := discover.Discover(a.gh, a.cfg.Owner, a.org)
			c.discoverErr = ""
			c.workflowErrors = found.Errors
			c.coverage = discoveryCoverage{Private: found.PrivateRepos, Scanned: found.Scanned, Failed: len(found.Errors), CheckedAt: time.Now().UTC()}
			if err != nil {
				c.coverage.State = "incomplete"
			} else if len(found.Errors) != 0 {
				c.coverage.State = "partial"
			} else {
				c.coverage.State = "complete"
			}
			if err != nil {
				c.discoverErr = err.Error()
			} else {
				c.repos, foundAt = found.Repos, time.Now()
				a.col.Repositories = names(found.Repos)
			}
		}
		if err := c.step(os.Stdout, time.Now().UTC(), a.col.Observe(a.providers)); err != nil {
			return err
		}
		beat.mark(time.Now())
		requests.flush(os.Stderr)
		<-tick.C
	}
}

// heartbeat answers /healthz with whether the loop finished a check recently.
// It reports the loop, not GitHub: a GitHub outage is logged, and restarting
// zerobill would not fix it.
type heartbeat struct {
	last       atomic.Int64
	staleAfter time.Duration
}

// staleAfter allows a slow first check, such as a cold month of Blacksmith jobs.
const staleAfter = 3 * config.DefaultCheckInterval

func (h *heartbeat) mark(t time.Time) { h.last.Store(t.UnixNano()) }

func (h *heartbeat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/healthz" {
		http.NotFound(w, r)
		return
	}
	since := time.Since(time.Unix(0, h.last.Load())).Round(time.Second)
	limit := h.staleAfter
	if limit == 0 {
		limit = staleAfter
	}
	if since > limit {
		http.Error(w, fmt.Sprintf("no check for %s", since), http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintf(w, "ok, last check %s ago\n", since)
}

func names(repos []discover.Repo) []string {
	out := make([]string, len(repos))
	for i, r := range repos {
		out[i] = r.Name
	}
	return out
}

// controller decides each interval and, with apply set, writes the decision.
type controller struct {
	a              *app
	apply          bool
	writer         *apply.Writer
	state          apply.State
	repos          []discover.Repo
	discoverErr    string
	workflowErrors []string
	coverage       discoveryCoverage
	lastCheck      time.Time
}

func newController(a *app, write bool) (*controller, error) {
	st, err := apply.LoadState(a.appliedPath())
	if err != nil {
		return nil, err
	}
	c := &controller{a: a, apply: write, state: st}
	if write {
		c.writer = &apply.Writer{GH: a.gh}
	}
	return c, nil
}

// line is one run log entry.
type line struct {
	Time         time.Time           `json:"time"`
	Apply        bool                `json:"apply"`
	Routes       []routeLine         `json:"routes"`
	Observed     map[string]observed `json:"observed"`
	Discovery    *discoveryCoverage  `json:"discovery,omitempty"`
	DelayedBy    string              `json:"check_delayed_by,omitempty"`
	Repositories int                 `json:"repositories"`
	Paused       map[string]string   `json:"paused,omitempty"`
	Errors       map[string]string   `json:"errors,omitempty"`
	Alerts       []capacityAlert     `json:"alerts,omitempty"`
	// Cancelled and Rerun list runs moved off a down runner, as owner/name#id.
	Cancelled []string          `json:"cancelled,omitempty"`
	Rerun     []string          `json:"rerun,omitempty"`
	Forced    []string          `json:"force_cancel_requested,omitempty"`
	Recovery  map[string]string `json:"recovery,omitempty"`
}

type discoveryCoverage struct {
	State     string    `json:"state"`
	CheckedAt time.Time `json:"checked_at"`
	Private   int       `json:"private_repositories"`
	Scanned   int       `json:"scanned"`
	Failed    int       `json:"failed"`
}

type routeLine struct {
	Variable string `json:"variable"`
	Provider string `json:"provider"`
	Label    string `json:"label"`
	Reason   string `json:"reason"`
	Held     bool   `json:"held,omitempty"`
	// Written lists repositories whose variable changed; null without --apply.
	Written []string          `json:"written"`
	Failed  map[string]string `json:"failed,omitempty"`
}

// observed is one provider's state in a log line.
type observed struct {
	Online       *bool                     `json:"online,omitempty"`
	UsedMinutes  float64                   `json:"used_minutes,omitempty"`
	Queue        *collect.QueueDiagnostics `json:"queue,omitempty"`
	OldestQueued string                    `json:"oldest_queued,omitempty"`
}

func (c *controller) step(w io.Writer, now time.Time, r collect.Result) error {
	l := line{Time: now, Apply: c.apply, Observed: map[string]observed{}, Repositories: len(c.repos), Errors: map[string]string{}}
	if c.coverage.State != "" {
		l.Discovery = &c.coverage
	}
	interval := c.a.cfg.CheckInterval
	if interval == 0 {
		interval = config.DefaultCheckInterval
	}
	if late := now.Sub(c.lastCheck) - interval; !c.lastCheck.IsZero() && late >= time.Second {
		l.DelayedBy = late.Round(time.Second).String()
	}
	c.lastCheck = now
	for k, v := range r.Errors {
		l.Errors[k] = v
	}
	if c.discoverErr != "" {
		l.Errors["discover"] = c.discoverErr
	}
	states := adjust(c.a, &c.state, now, r.States, l.Errors)
	for _, p := range c.a.providers {
		s, ok := states[p.Name]
		if !ok {
			continue
		}
		o := observed{UsedMinutes: s.UsedMinutes}
		if p.Kind == policy.SelfHosted {
			o.Online = &s.Online
			if queue, ok := r.Queues[p.Name]; ok {
				o.Queue = &queue
			}
			if s.OldestQueued > 0 {
				o.OldestQueued = s.OldestQueued.Round(time.Second).String()
			}
		}
		l.Observed[p.Name] = o
		if s.Paused != "" {
			if l.Paused == nil {
				l.Paused = map[string]string{}
			}
			l.Paused[p.Name] = s.Paused
		}
	}
	if c.state.Routes == nil {
		c.state.Routes = map[string]policy.Applied{}
	}
	// An interval with a failed read writes nothing, so a transient error cannot
	// move jobs onto a metered provider.
	readsOK := len(l.Errors) == 0
	write := c.apply && readsOK
	if c.apply && !write {
		l.Errors["write"] = "skipped: a read failed"
	}
	targets := apply.Targets{}
	var changes []string
	for _, rt := range c.a.routes {
		applied := c.state.Routes[rt.Variable]
		d, held := policy.Hold(policy.Decide(rt, states), applied, rt, states, now)
		rl := routeLine{Variable: rt.Variable, Provider: d.Provider, Label: d.Label, Reason: d.Reason, Held: held}
		// Without --apply the decision counts as applied in memory, so the log
		// shows the holds a real run would make.
		took := !c.apply
		if write {
			eligible := c.replacementEligible(rl, states)
			rl.Written = []string{}
			for _, repo := range c.repos {
				if !reads(repo, rt.Variable) {
					continue
				}
				changed, err := c.writer.Set(repo.Name, rt.Variable, d.Label)
				if err != nil {
					if rl.Failed == nil {
						rl.Failed = map[string]string{}
					}
					rl.Failed[repo.Name] = err.Error()
					l.Errors["write "+repo.Name+"/"+rt.Variable] = err.Error()
					continue
				}
				if targets[repo.Name] == nil {
					targets[repo.Name] = map[string]string{}
				}
				targets[repo.Name][rt.Variable] = ""
				if eligible {
					targets[repo.Name][rt.Variable] = d.Label
				}
				// A partial write still moves jobs, so it starts the hold.
				took = true
				if changed {
					rl.Written = append(rl.Written, repo.Name)
				}
			}
		}
		if took {
			if d.Label != applied.Label || d.Provider != applied.Provider {
				applied = policy.Applied{Provider: d.Provider, Label: d.Label, Since: now}
				changes = append(changes, fmt.Sprintf("%s → %s (%s)", rt.Variable, d.Label, d.Reason))
			}
			if write {
				applied.Owner = c.a.cfg.Owner
			}
			c.state.Routes[rt.Variable] = applied
		}
		l.Routes = append(l.Routes, rl)
	}
	if c.apply {
		checkpoint := func() error { return apply.SaveState(c.a.appliedPath(), c.state) }
		moves, err := c.writer.Move(&c.state, c.stuck(states, r), targets, now, checkpoint)
		l.Cancelled, l.Forced, l.Rerun, l.Recovery = moves.Cancelled, moves.Forced, moves.Rerun, moves.Recovery
		if err != nil {
			l.Errors["move"] = err.Error()
		}
	}
	attention := false
	for k, m := range c.state.Cancelled {
		if l.Recovery == nil {
			l.Recovery = map[string]string{}
		}
		if !m.NeedsAttention() {
			if _, exists := l.Recovery[k]; !exists {
				l.Recovery[k] = fmt.Sprintf("attempt %d: %s", m.Attempt, m.PendingStatus())
			}
			continue
		}
		attention = true
		message := fmt.Sprintf("zerobill: recovery needs attention: https://github.com/%s/actions/runs/%d attempt %d: %s", m.Repo, m.ID, m.Attempt, m.Problem)
		l.Recovery[k] = message
		if write && !m.Alerted && c.a.cfg.Notify != "" {
			if err := notify.Send(c.a.cfg.Notify, message); err != nil {
				l.Errors["notify recovery "+k] = err.Error()
			} else {
				m.Alerted = true
				c.state.Cancelled[k] = m
			}
		}
	}
	if len(changes) > 0 && c.a.cfg.Notify != "" {
		msg := "zerobill: " + strings.Join(changes, "; ")
		if !c.apply {
			msg += " [log only]"
		}
		if err := notify.Send(c.a.cfg.Notify, msg); err != nil {
			l.Errors["notify"] = err.Error()
		}
	}
	// Low-capacity warnings need complete reads; failed sends remain eligible next check.
	if readsOK {
		l.Alerts = c.capacityAlerts(now, states)
		if c.apply && c.a.cfg.Notify != "" {
			for _, alert := range l.Alerts {
				if c.state.Alerts[alert.Provider] {
					continue
				}
				if err := notify.Send(c.a.cfg.Notify, alert.Message); err != nil {
					l.Errors["notify capacity "+alert.Provider] = err.Error()
				} else {
					c.state.Alerts[alert.Provider] = true
				}
			}
		}
	}
	if c.apply {
		if err := apply.SaveState(c.a.appliedPath(), c.state); err != nil {
			l.Errors["state"] = err.Error()
		}
	}
	if len(c.workflowErrors) > 0 {
		l.Errors["workflows"] = strings.Join(c.workflowErrors, "; ")
	}
	if c.apply {
		if err := c.saveStatus(now, states, len(l.Errors) > 0 || attention); err != nil {
			l.Errors["status"] = err.Error()
		}
	}
	if len(l.Errors) == 0 {
		l.Errors = nil
	}
	return json.NewEncoder(w).Encode(l)
}

func (c *controller) replacementEligible(route routeLine, states map[string]policy.State) bool {
	for _, p := range c.a.providers {
		if p.Name == route.Provider && p.Label == route.Label {
			if state, ok := states[p.Name]; ok {
				usable, _ := policy.Usable(p, state)
				return usable && p.Kind == policy.Metered
			}
		}
	}
	return false
}

func reads(r discover.Repo, variable string) bool {
	for _, v := range r.Vars {
		if v == variable {
			return true
		}
	}
	return false
}

// adjust adds what zerobill remembers to the observed states: pauses, and a
// queue stall that keeps the self-hosted runner down until a runner is online
// again, or until it is retried: after RetryAfter for a pool that only registers
// runners for jobs, after WarmRetryAfter for a warm pool, whose return shows up
// without a probe job. Problems are added to errs.
func adjust(a *app, st *apply.State, now time.Time, observed map[string]policy.State, errs map[string]string) map[string]policy.State {
	set, err := pauses.Load(a.pausePath())
	if err != nil {
		errs["pause"] = err.Error()
	}
	paused := set.Active(now)
	if st.Stalled == nil {
		st.Stalled = map[string]time.Time{}
	}
	if st.IdleSince == nil {
		st.IdleSince = map[string]time.Time{}
	}
	if st.Warm == nil {
		st.Warm = map[string]bool{}
	}
	checkInterval := a.cfg.CheckInterval
	if checkInterval == 0 {
		checkInterval = config.DefaultCheckInterval
	}
	queueTimeout := a.cfg.QueueTimeout
	if queueTimeout == 0 {
		queueTimeout = config.DefaultQueueTimeout
	}
	states := make(map[string]policy.State, len(observed))
	for _, p := range a.providers {
		s, ok := observed[p.Name]
		if !ok {
			continue
		}
		if p.Kind == policy.SelfHosted {
			// A per-job runner is idle only for moments, so a pool counts as warm
			// after an idle runner is seen on two consecutive checks. The window
			// allows for a late check; an older sighting starts over.
			since, ok := st.IdleSince[p.Label]
			switch d := now.Sub(since); {
			case !s.Idle:
				delete(st.IdleSince, p.Label)
			case ok && d >= checkInterval/2 && d < 2*checkInterval:
				st.Warm[p.Label] = true
			case !ok || d >= 2*checkInterval:
				st.IdleSince[p.Label] = now
			}
			_, stalled := st.Stalled[p.Name]
			switch {
			case s.Online:
				delete(st.Stalled, p.Name)
			case s.OldestQueued > queueTimeout && !stalled:
				st.Stalled[p.Name] = now
			}
			retry := policy.RetryAfter
			if st.Warm[p.Label] {
				retry = policy.WarmRetryAfter
			}
			if at, ok := st.Stalled[p.Name]; ok && now.Sub(at) >= retry {
				delete(st.Stalled, p.Name)
			}
			_, s.Stalled = st.Stalled[p.Name]
		}
		s.Paused = paused[p.Name]
		states[p.Name] = s
	}
	return states
}

// stuck returns candidate runs for an unusable self-hosted pool.
func (c *controller) stuck(states map[string]policy.State, r collect.Result) []collect.Run {
	var runs []collect.Run
	for _, p := range c.a.providers {
		if p.Kind != policy.SelfHosted {
			continue
		}
		if s, ok := states[p.Name]; ok {
			if usable, _ := policy.Usable(p, s); !usable {
				runs = append(runs, r.Movable[p.Name]...)
			}
		}
	}
	return runs
}

type capacityAlert struct {
	Provider string `json:"provider"`
	Message  string `json:"message"`
}

func (c *controller) capacityAlerts(now time.Time, states map[string]policy.State) []capacityAlert {
	month := now.UTC().Format("2006-01")
	if c.state.AlertMonth != month || c.state.AlertOwner != c.a.cfg.Owner {
		c.state.AlertMonth, c.state.AlertOwner = month, c.a.cfg.Owner
		c.state.Alerts = map[string]bool{}
	}
	if c.state.Alerts == nil {
		c.state.Alerts = map[string]bool{}
	}
	threshold := c.a.cfg.AlertRemainingPercent
	if threshold == 0 {
		return nil
	}
	var alerts []capacityAlert
	for _, p := range c.a.providers {
		s, ok := states[p.Name]
		if !ok || p.Kind != policy.Metered || p.Monthly <= 0 {
			continue
		}
		remaining := max(0, p.Monthly-s.UsedMinutes)
		percent := remaining * 100 / p.Monthly
		if percent > float64(threshold) {
			continue
		}
		action := "Free capacity is running low."
		if c.a.cfg.SelfHostedLabel != "" {
			action = "Turn on your self-hosted runners if needed."
		}
		msg := fmt.Sprintf("zerobill: %s has %.0f%% of its monthly free allowance left (%.0f min); reserve %d%%. %s Provider spending limits are still required to prevent charges.", p.Name, percent, remaining, p.ReservePercent, action)
		if p.Name == "blacksmith" {
			msg += " Blacksmith usage is estimated."
		}
		alerts = append(alerts, capacityAlert{Provider: p.Name, Message: msg})
	}
	return alerts
}
