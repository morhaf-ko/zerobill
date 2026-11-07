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

// Set sets the Actions variable name in repo, owner/name, to label, writing
// only when it differs. It reports whether it wrote.
func (w *Writer) Set(repo, name, label string) (bool, error) {
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

// State is what run keeps between intervals.
type State struct {
	// Routes holds the label applied per route, for the minimum hold.
	Routes map[string]policy.Applied `json:"routes,omitempty"`
	// Stalled holds when each on-demand provider last had a queue stall with no
	// runner online.
	Stalled map[string]time.Time `json:"stalled,omitempty"`
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
// variables, so it lands on the applied label. checkpoint persists st after each
// cancel or rerun, so a crash cannot lose a cancelled run or the loop guard. It
// returns the runs it cancelled and reran as "owner/name#id".
func (w *Writer) Move(st *State, stuck []collect.Run, now time.Time, checkpoint func() error) (cancelled, rerun []string, err error) {
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
		err := w.GH.Get(fmt.Sprintf("/repos/%s/actions/runs/%d", m.Repo, m.ID), &run)
		waiting := false
		switch {
		case err != nil:
		case run.RunAttempt != m.Attempt:
			// Someone else reran it; it still counts as moved.
			delete(st.Cancelled, k)
			st.Rerun[k] = Moved{Run: m.Run, At: now}
			continue
		case run.Status != "completed":
			waiting = true
		default:
			err = w.GH.Send(http.MethodPost, fmt.Sprintf("/repos/%s/actions/runs/%d/rerun-failed-jobs", m.Repo, m.ID), nil)
			if err == nil {
				delete(st.Cancelled, k)
				st.Rerun[k] = Moved{Run: m.Run, At: now}
				rerun = append(rerun, k)
				if err := checkpoint(); err != nil {
					return cancelled, rerun, errors.Join(append(errs, err)...)
				}
				continue
			}
		}
		if now.Sub(m.At) > cancelTimeout {
			delete(st.Cancelled, k)
			why := fmt.Sprintf("run still %s", run.Status)
			if !waiting {
				why = err.Error()
			}
			errs = append(errs, fmt.Errorf("gave up on %s cancelled %s ago: %s", k, now.Sub(m.At).Round(time.Minute), why))
		} else if err != nil {
			errs = append(errs, err)
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
			// The run cannot be cancelled now, usually because it finished; never retry it.
			st.Rerun[k] = Moved{Run: r, At: now}
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		st.Cancelled[k] = Moved{Run: r, At: now}
		cancelled = append(cancelled, k)
		if err := checkpoint(); err != nil {
			sort.Strings(rerun)
			return cancelled, rerun, errors.Join(append(errs, err)...)
		}
	}
	sort.Strings(rerun)
	return cancelled, rerun, errors.Join(errs...)
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
