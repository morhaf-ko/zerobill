package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/morhaf-ko/zerobill/internal/apply"
	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/discover"
	"github.com/morhaf-ko/zerobill/internal/notify"
	pauses "github.com/morhaf-ko/zerobill/internal/pause"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

const (
	interval = 5 * time.Minute
	// rediscover is how often run rescans workflows; every interval would spend
	// the API rate limit on large organizations.
	rediscover = time.Hour
)

func run(args []string) error {
	write := false
	for _, arg := range args {
		if arg != "--apply" {
			return fmt.Errorf("unknown argument %q; run takes only --apply", arg)
		}
		write = true
	}
	a, err := load(true)
	if err != nil {
		return err
	}
	c, err := newController(a, write)
	if err != nil {
		return err
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	var foundAt time.Time
	for {
		if time.Since(foundAt) >= rediscover {
			found, err := discover.Discover(a.gh, a.cfg.Owner, a.org)
			c.discoverErr = ""
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
		<-tick.C
	}
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
	a           *app
	apply       bool
	writer      *apply.Writer
	state       apply.State
	repos       []discover.Repo
	discoverErr string
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
	Repositories int                 `json:"repositories"`
	Paused       map[string]string   `json:"paused,omitempty"`
	Errors       map[string]string   `json:"errors,omitempty"`
	// Cancelled and Rerun list runs moved off a down runner, as owner/name#id.
	Cancelled []string `json:"cancelled,omitempty"`
	Rerun     []string `json:"rerun,omitempty"`
}

type routeLine struct {
	Variable string `json:"variable"`
	Provider string `json:"provider"`
	Label    string `json:"label"`
	Reason   string `json:"reason"`
	Held     bool   `json:"held,omitempty"`
	// Written lists repositories whose variable changed; null without --apply.
	Written []string `json:"written"`
}

// observed is one provider's state in a log line.
type observed struct {
	Online       *bool   `json:"online,omitempty"`
	UsedMinutes  float64 `json:"used_minutes,omitempty"`
	OldestQueued string  `json:"oldest_queued,omitempty"`
}

func (c *controller) step(w io.Writer, now time.Time, r collect.Result) error {
	l := line{Time: now, Apply: c.apply, Observed: map[string]observed{}, Repositories: len(c.repos), Errors: map[string]string{}}
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
	write := c.apply && len(l.Errors) == 0
	if c.apply && !write {
		l.Errors["write"] = "skipped: a read failed"
	}
	allWritten := true
	chosen := map[string]bool{}
	var changes []string
	for _, rt := range c.a.routes {
		applied := c.state.Routes[rt.Variable]
		d, held := policy.Hold(policy.Decide(rt, states), applied, rt, states, now)
		rl := routeLine{Variable: rt.Variable, Provider: d.Provider, Label: d.Label, Reason: d.Reason, Held: held}
		chosen[d.Label] = true
		// Without --apply the decision counts as applied in memory, so the log
		// shows the holds a real run would make.
		took := !c.apply
		if write {
			rl.Written = []string{}
			for _, repo := range c.repos {
				if !reads(repo, rt.Variable) {
					continue
				}
				changed, err := c.writer.Set(repo.Name, rt.Variable, d.Label)
				if err != nil {
					l.Errors["write "+repo.Name] = err.Error()
					allWritten = false
					continue
				}
				// A partial write still moves jobs, so it starts the hold.
				took = true
				if changed {
					rl.Written = append(rl.Written, repo.Name)
				}
			}
		}
		if took && (d.Label != applied.Label || d.Provider != applied.Provider) {
			c.state.Routes[rt.Variable] = policy.Applied{Provider: d.Provider, Label: d.Label, Since: now}
			changes = append(changes, fmt.Sprintf("%s → %s (%s)", rt.Variable, d.Label, d.Reason))
		}
		l.Routes = append(l.Routes, rl)
	}
	if write {
		// Runs move only once every variable holds its new label, so a rerun job reads it.
		if allWritten {
			checkpoint := func() error { return apply.SaveState(c.a.appliedPath(), c.state) }
			var err error
			l.Cancelled, l.Rerun, err = c.writer.Move(&c.state, c.stuck(chosen, states, r), now, checkpoint)
			if err != nil {
				l.Errors["move"] = err.Error()
			}
		}
		if err := apply.SaveState(c.a.appliedPath(), c.state); err != nil {
			l.Errors["state"] = err.Error()
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
	if len(l.Errors) == 0 {
		l.Errors = nil
	}
	return json.NewEncoder(w).Encode(l)
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
// queue stall that keeps the self-hosted runner down. A pool seen with idle
// runners stays down until one is online again, so its return is noticed
// without a probe job; a pool that only registers runners for jobs is retried
// after RetryAfter. Problems are added to errs.
func adjust(a *app, st *apply.State, now time.Time, observed map[string]policy.State, errs map[string]string) map[string]policy.State {
	set, err := pauses.Load(a.pausePath())
	if err != nil {
		errs["pause"] = err.Error()
	}
	paused := set.Active(now)
	if st.Stalled == nil {
		st.Stalled = map[string]time.Time{}
	}
	if st.Idle == nil {
		st.Idle = map[string]bool{}
	}
	states := make(map[string]policy.State, len(observed))
	for _, p := range a.providers {
		s, ok := observed[p.Name]
		if !ok {
			continue
		}
		if p.Kind == policy.SelfHosted {
			switch {
			case s.Online:
				delete(st.Stalled, p.Name)
				if s.OldestQueued == 0 {
					st.Idle[p.Label] = true
				}
			case s.OldestQueued > policy.MaxQueueWait:
				st.Stalled[p.Name] = now
			}
			if at, ok := st.Stalled[p.Name]; ok && !st.Idle[p.Label] && now.Sub(at) >= policy.RetryAfter {
				delete(st.Stalled, p.Name)
			}
			_, s.Stalled = st.Stalled[p.Name]
		}
		s.Paused = paused[p.Name]
		states[p.Name] = s
	}
	return states
}

// stuck returns movable runs queued on the self-hosted label when no route
// chose it and the runner cannot take jobs now.
func (c *controller) stuck(chosen map[string]bool, states map[string]policy.State, r collect.Result) []collect.Run {
	var runs []collect.Run
	for _, p := range c.a.providers {
		if p.Kind != policy.SelfHosted || chosen[p.Label] {
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
