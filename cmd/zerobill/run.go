package main

import (
	"encoding/json"
	"flag"
	"io"
	"math"
	"os"
	"time"

	"github.com/morhaf-ko/zerobill/internal/apply"
	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/discover"
	"github.com/morhaf-ko/zerobill/internal/override"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

// rediscover is how often run rescans the owner's workflows; scanning every
// interval would spend the API rate limit on large organizations.
const rediscover = time.Hour

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	write := fs.Bool("apply", false, "write the variables and move stuck jobs; without it run only logs")
	interval := fs.Duration("interval", 5*time.Minute, "time between decisions")
	if err := flags(fs, args); err != nil {
		return err
	}
	a, err := load()
	if err != nil {
		return err
	}
	c, err := newController(a, *write)
	if err != nil {
		return err
	}
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	var found discover.Result
	var foundAt time.Time
	for {
		now := time.Now().UTC()
		if now.Sub(foundAt) >= rediscover {
			if r, err := discover.Discover(a.gh, a.cfg.Owner, a.org); err != nil {
				c.discoverErr = err.Error()
			} else {
				found, foundAt, c.discoverErr = r, now, ""
			}
		}
		c.repos = found.Repos
		a.col.Repositories = names(found.Repos)
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
	Repositories []string            `json:"repositories"`
	Paused       map[string]string   `json:"paused,omitempty"`
	Errors       map[string]string   `json:"errors,omitempty"`
	Warnings     map[string]string   `json:"warnings,omitempty"`
	SavedUSD     float64             `json:"saved_usd_estimate"`
	// Cancelled and Rerun list runs moved off a down self-hosted runner, as owner/name#id.
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

func (c *controller) step(w io.Writer, now time.Time, r collect.Result) error {
	l := line{Time: now, Apply: c.apply, Observed: snapshot(c.a.providers, r.States), Repositories: names(c.repos), Errors: map[string]string{}}
	for k, v := range r.Errors {
		l.Errors[k] = v
	}
	if c.discoverErr != "" {
		l.Errors["discover"] = c.discoverErr
	}
	if len(r.Warnings) > 0 {
		l.Warnings = r.Warnings
	}
	states := c.adjust(now, r.States, l.Errors)
	for name, s := range states {
		if s.Paused != "" {
			if l.Paused == nil {
				l.Paused = map[string]string{}
			}
			l.Paused[name] = s.Paused
		}
	}
	l.SavedUSD = math.Round(policy.Saved(c.a.providers, r.States)*100) / 100
	if c.state.Routes == nil {
		c.state.Routes = map[string]policy.Applied{}
	}
	// An interval with a failed collector writes nothing, so a transient error
	// cannot move jobs onto a metered provider.
	write := c.apply && len(l.Errors) == 0
	if c.apply && !write {
		l.Errors["write"] = "skipped: a collector failed"
	}
	allWritten := true
	chosen := map[string]bool{}
	for _, rt := range c.a.routes {
		d := policy.Decide(rt, states)
		applied := c.state.Routes[rt.Variable]
		var held bool
		d, held = policy.Hold(d, applied, rt, states, now)
		rl := routeLine{Variable: rt.Variable, Provider: d.Provider, Label: d.Label, Reason: d.Reason, Held: held}
		chosen[d.Label] = true
		changed := d.Label != applied.Label || d.Provider != applied.Provider
		wrote := !c.apply
		if write {
			rl.Written = []string{}
			for _, repo := range c.repos {
				if !reads(repo, rt.Variable) {
					continue
				}
				ok, err := c.writer.Set(repo.Name, rt.Variable, d.Label)
				switch {
				case err != nil:
					l.Errors["write "+repo.Name] = err.Error()
					allWritten = false
				case ok:
					rl.Written = append(rl.Written, repo.Name)
					wrote = true
				default:
					wrote = true
				}
			}
			// A partial write still moves jobs, so it starts the hold.
			wrote = wrote || len(rl.Written) > 0
		}
		if changed && wrote {
			c.state.Routes[rt.Variable] = policy.Applied{Provider: d.Provider, Label: d.Label, Since: now}
		}
		l.Routes = append(l.Routes, rl)
	}
	// Runs move only once every variable holds its new label, so a rerun job reads it.
	if write && allWritten {
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
	if len(l.Errors) == 0 {
		l.Errors = nil
	}
	return json.NewEncoder(w).Encode(l)
}

// observed is one provider's state in a log line.
type observed struct {
	Online       *bool   `json:"online,omitempty"`
	UsedMinutes  float64 `json:"used_minutes"`
	OldestQueued string  `json:"oldest_queued,omitempty"`
}

func snapshot(providers []policy.Provider, states map[string]policy.State) map[string]observed {
	out := map[string]observed{}
	for _, p := range providers {
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
		out[p.Name] = o
	}
	return out
}

func reads(r discover.Repo, variable string) bool {
	for _, v := range r.Vars {
		if v == variable {
			return true
		}
	}
	return false
}

// adjust adds what zerobill remembers to the observed states: pauses and the
// queue-stall latch of self-hosted runners.
func (c *controller) adjust(now time.Time, observed map[string]policy.State, errs map[string]string) map[string]policy.State {
	set, err := override.Load(c.a.pausePath())
	if err != nil {
		errs["pause"] = err.Error()
	}
	paused := set.Active(now)
	if c.state.Stalled == nil {
		c.state.Stalled = map[string]time.Time{}
	}
	states := make(map[string]policy.State, len(observed))
	for _, p := range c.a.providers {
		s, ok := observed[p.Name]
		if !ok {
			continue
		}
		if p.Kind == policy.SelfHosted {
			switch {
			case s.Online:
				delete(c.state.Stalled, p.Name)
			case s.OldestQueued > policy.MaxQueueWait:
				c.state.Stalled[p.Name] = now
			}
			if at, ok := c.state.Stalled[p.Name]; ok {
				if now.Sub(at) < policy.RetryAfter {
					s.Stalled = true
				} else {
					delete(c.state.Stalled, p.Name)
				}
			}
		}
		s.Paused = paused[p.Name]
		states[p.Name] = s
	}
	return states
}

// stuck returns movable runs queued on a self-hosted label that no route chose
// and whose runner cannot take jobs now.
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
