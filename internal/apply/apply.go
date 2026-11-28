// Package apply writes the chosen runner label to repository variables.
package apply

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/morhaf-ko/zerobill/internal/atomicfile"
	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/github"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

// Writer sets repository variables through the GitHub REST API.
type Writer struct {
	GH      *github.Client
	checked map[variableKey]variableCheck
}

type variableKey struct{ repo, name string }

type variableCheck struct {
	label string
	at    time.Time
}

// Reconcile checks stable variables every five minutes and checks changes immediately.
func (w *Writer) Reconcile(repo, name, label string, now time.Time, force bool) (bool, error) {
	key := variableKey{repo, name}
	if prior, ok := w.checked[key]; ok && !force && prior.label == label &&
		!now.Before(prior.at) && now.Sub(prior.at) < 5*time.Minute {
		return false, nil
	}
	delete(w.checked, key)
	changed, err := w.set(repo, name, label, !force)
	if err != nil {
		return false, err
	}
	if w.checked == nil {
		w.checked = make(map[variableKey]variableCheck)
	}
	w.checked[key] = variableCheck{label, now}
	return changed, nil
}

// Forget removes the observation for a variable no longer managed by this writer.
func (w *Writer) Forget(repo, name string) {
	delete(w.checked, variableKey{repo, name})
}

func (w *Writer) variable(repo, name string) (string, error) {
	var current struct {
		Value string `json:"value"`
	}
	err := w.GH.Get("/repos/"+repo+"/actions/variables/"+url.PathEscape(name), &current)
	return current.Value, err
}

// Set sets the Actions variable name in repo, owner/name, to label, writing
// only when it differs. It reports whether it wrote.
func (w *Writer) Set(repo, name, label string) (bool, error) {
	return w.set(repo, name, label, false)
}

func (w *Writer) set(repo, name, label string, conditional bool) (bool, error) {
	base := "/repos/" + repo + "/actions/variables"
	path := base + "/" + url.PathEscape(name)
	body := map[string]string{"name": name, "value": label}
	var current struct {
		Value string `json:"value"`
	}
	get := w.GH.Get
	if conditional {
		get = w.GH.ConditionalGet
	}
	err := get(path, &current)
	switch {
	case github.IsNotFound(err):
		return true, w.GH.Send(http.MethodPost, base, body)
	case err != nil:
		return false, err
	case current.Value == label:
		return false, nil
	default:
		return true, w.GH.Send(http.MethodPatch, path, body)
	}
}

// State is what run keeps between intervals.
type State struct {
	// Alerts records successful low-capacity warnings for the owner and UTC billing month.
	Alerts     map[string]bool `json:"alerts,omitempty"`
	AlertMonth string          `json:"alert_month,omitempty"`
	AlertOwner string          `json:"alert_owner,omitempty"`
	// Routes holds the label applied per route, for the minimum hold.
	Routes map[string]policy.Applied `json:"routes,omitempty"`
	// Stalled holds when the self-hosted runner last had a queue stall with no
	// runner online; it is cleared when one is online.
	Stalled map[string]time.Time `json:"stalled,omitempty"`
	// IdleSince holds when a runner with the label was first seen idle in the
	// current unbroken run of checks. Warm marks labels whose pool kept an idle
	// runner across two checks, so its return shows up without a probe job.
	IdleSince map[string]time.Time `json:"idle_since,omitempty"`
	Warm      map[string]bool      `json:"warm,omitempty"`
	// Cancelled holds pending recoveries and failures awaiting operator attention.
	Cancelled map[string]Moved `json:"cancelled,omitempty"`
	// Rerun holds handled runs, including externally completed attempts, to prevent loops.
	Rerun map[string]Moved `json:"rerun,omitempty"`
}

// Moved tracks one owned recovery. Empty Stage means normal cancellation was accepted.
type Moved struct {
	collect.Run
	At              time.Time     `json:"at"`
	Stage           string        `json:"stage,omitempty"`
	StageAt         time.Time     `json:"stage_at,omitempty"`
	Jobs            []MigratedJob `json:"jobs,omitempty"`
	LabelsConfirmed bool          `json:"labels_confirmed,omitempty"`
	Problem         string        `json:"problem,omitempty"`
	Alerted         bool          `json:"alerted,omitempty"`
}

// LoadState reads run state; a missing file means none.
func LoadState(path string) (State, error) {
	var st State
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("%s: %w", path, err)
	}
	return st, nil
}

// SaveState records run state.
func SaveState(path string, st State) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, raw)
}
