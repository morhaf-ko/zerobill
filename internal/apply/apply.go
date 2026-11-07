// Package apply writes the chosen runner label to repository variables.
package apply

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"time"

	"github.com/morhaf-ko/zerobill/internal/atomicfile"
	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/github"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

// Writer sets repository variables through the GitHub REST API.
type Writer struct {
	GH *github.Client
}

// Sync sets each variable in each repository, given as owner/name, to label.
// Variables that already hold label are not written. It returns the variables it
// wrote as "owner/name VARIABLE" and every failure joined.
func (w *Writer) Sync(repos, vars []string, label string) ([]string, error) {
	var written []string
	var errs []error
	for _, repo := range repos {
		for _, name := range vars {
			changed, err := w.set(repo, name, label)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if changed {
				written = append(written, repo+" "+name)
			}
		}
	}
	return written, errors.Join(errs...)
}

func (w *Writer) set(repo, name, label string) (bool, error) {
	base := "/repos/" + repo + "/actions/variables"
	path := base + "/" + url.PathEscape(name)
	var cur struct {
		Value string `json:"value"`
	}
	body := map[string]string{"name": name, "value": label}
	err := w.GH.Get(path, &cur)
	switch {
	case github.IsNotFound(err):
		return true, w.GH.Send(http.MethodPost, base, body)
	case err != nil:
		return false, err
	case cur.Value == label:
		return false, nil
	default:
		return true, w.GH.Send(http.MethodPatch, path, body)
	}
}

// State is what switch mode keeps between intervals.
type State struct {
	policy.Applied
	// Cancelled holds runs cancelled to leave a stale label, awaiting their rerun.
	Cancelled map[string]Moved `json:"cancelled,omitempty"`
	// Rerun holds runs already moved once; they are never moved again, so a job
	// whose runs-on is a literal label cannot loop.
	Rerun map[string]Moved `json:"rerun,omitempty"`
}

// Moved is a run zerobill cancelled, and when.
type Moved struct {
	collect.Run
	At time.Time `json:"at"`
}

// cancelTimeout drops a cancelled run that never completed, so it stops being polled.
const cancelTimeout = time.Hour

// rerunMemory exceeds GitHub's 24-hour queue limit, after which a run cannot still be stuck.
const rerunMemory = 48 * time.Hour

func key(r collect.Run) string { return fmt.Sprintf("%s#%d", r.Repo, r.ID) }

// Move reruns runs cancelled in an earlier interval once they completed, then
// cancels stuck runs that were never moved. A rerun job reads the current
// variables, so it lands on the applied label. It returns the runs it cancelled
// and reran as "owner/name#id".
func (w *Writer) Move(st *State, stuck []collect.Run, now time.Time) (cancelled, rerun []string, err error) {
	if st.Cancelled == nil {
		st.Cancelled = map[string]Moved{}
	}
	if st.Rerun == nil {
		st.Rerun = map[string]Moved{}
	}
	var errs []error
	for k, m := range st.Cancelled {
		var run struct {
			Status     string `json:"status"`
			RunAttempt int    `json:"run_attempt"`
		}
		if err := w.GH.Get(fmt.Sprintf("/repos/%s/actions/runs/%d", m.Repo, m.ID), &run); err != nil {
			errs = append(errs, err)
			continue
		}
		switch {
		case run.RunAttempt != m.Attempt:
			// Someone else reran it.
			delete(st.Cancelled, k)
		case run.Status != "completed":
			if now.Sub(m.At) > cancelTimeout {
				delete(st.Cancelled, k)
				errs = append(errs, fmt.Errorf("%s: cancelled %s ago and still %s; gave up", k, now.Sub(m.At).Round(time.Minute), run.Status))
			}
		default:
			if err := w.GH.Send(http.MethodPost, fmt.Sprintf("/repos/%s/actions/runs/%d/rerun-failed-jobs", m.Repo, m.ID), nil); err != nil {
				errs = append(errs, err)
				continue
			}
			delete(st.Cancelled, k)
			st.Rerun[k] = Moved{Run: m.Run, At: now}
			rerun = append(rerun, k)
		}
	}
	for k, m := range st.Rerun {
		if now.Sub(m.At) > rerunMemory {
			delete(st.Rerun, k)
		}
	}
	for _, r := range stuck {
		k := key(r)
		if _, ok := st.Cancelled[k]; ok {
			continue
		}
		if _, ok := st.Rerun[k]; ok {
			continue
		}
		err := w.GH.Send(http.MethodPost, fmt.Sprintf("/repos/%s/actions/runs/%d/cancel", r.Repo, r.ID), nil)
		var se *github.StatusError
		if errors.As(err, &se) && se.Code == http.StatusConflict {
			// The run finished between the scan and the cancel.
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		st.Cancelled[k] = Moved{Run: r, At: now}
		cancelled = append(cancelled, k)
	}
	sort.Strings(rerun)
	return cancelled, rerun, errors.Join(errs...)
}

// LoadState reads switch-mode state; a missing file means none.
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

// SaveState records switch-mode state.
func SaveState(path string, st State) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, raw)
}
