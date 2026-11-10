package apply

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/morhaf-ko/zerobill/internal/collect"
)

const (
	recoveryWait    = 5 * time.Minute
	rerunMemory     = 48 * time.Hour
	cancelRequested = "cancel_requested"
	forceRequested  = "force_requested"
	rerunRequested  = "rerun_requested"
	needsAttention  = "needs_attention"
)

type Moves struct {
	Cancelled, Forced, Rerun []string
}

type remoteRun struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Attempt    int    `json:"run_attempt"`
}

type recoveryJob struct {
	ID         int64    `json:"id"`
	Status     string   `json:"status"`
	Conclusion string   `json:"conclusion"`
	Labels     []string `json:"labels"`
	Runner     string   `json:"runner_name"`
	RunnerID   int64    `json:"runner_id"`
	Steps      []struct {
		Status    string     `json:"status"`
		StartedAt *time.Time `json:"started_at"`
	} `json:"steps"`
}

func key(r collect.Run) string     { return fmt.Sprintf("%s#%d", r.Repo, r.ID) }
func runPath(r collect.Run) string { return fmt.Sprintf("/repos/%s/actions/runs/%d", r.Repo, r.ID) }

func (w *Writer) run(r collect.Run) (remoteRun, error) {
	var out remoteRun
	err := w.GH.Get(runPath(r), &out)
	if err == nil && (out.Attempt < 1 || out.Status == "") {
		err = fmt.Errorf("%s: incomplete run metadata", key(r))
	}
	return out, err
}

func (w *Writer) jobs(r collect.Run) ([]recoveryJob, error) {
	var jobs []recoveryJob
	err := w.GH.Paginate(fmt.Sprintf("%s/attempts/%d/jobs?per_page=100", runPath(r), r.Attempt), func(raw []byte) error {
		var page struct {
			Jobs []recoveryJob `json:"jobs"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		jobs = append(jobs, page.Jobs...)
		return nil
	})
	return jobs, err
}

func unstarted(j recoveryJob) bool {
	// Queued jobs can have started_at; runner assignment and steps reveal execution.
	if j.ID == 0 || j.Runner != "" || j.RunnerID != 0 {
		return false
	}
	for _, s := range j.Steps {
		if s.StartedAt != nil || (s.Status != "pending" && s.Status != "queued") {
			return false
		}
	}
	return j.Status == "queued" || (j.Status == "completed" && (j.Conclusion == "cancelled" || j.Conclusion == "skipped"))
}

func movable(jobs []recoveryJob, label string) bool {
	queued := false
	for _, j := range jobs {
		if j.Status == "queued" && unstarted(j) && slices.Contains(j.Labels, label) {
			queued = true
			continue
		}
		if j.Status != "completed" || (j.Conclusion != "success" && j.Conclusion != "skipped" && j.Conclusion != "neutral") {
			return false
		}
	}
	return label != "" && queued
}

func (m Moved) since() time.Time {
	if !m.StageAt.IsZero() {
		return m.StageAt
	}
	return m.At
}

func (m Moved) NeedsAttention() bool { return m.Stage == needsAttention }

// Move checkpoints mutation intent before each POST. An uncertain POST is reconciled,
// never resubmitted; GitHub offers no atomic attempt condition on these endpoints.
func (w *Writer) Move(st *State, stuck []collect.Run, targets map[string][]string, now time.Time, checkpoint func() error) (Moves, error) {
	var out Moves
	if st.Cancelled == nil {
		st.Cancelled = map[string]Moved{}
	}
	if st.Rerun == nil {
		st.Rerun = map[string]Moved{}
	}
	save := func(k string, m Moved) error {
		previous, exists := st.Cancelled[k]
		st.Cancelled[k] = m
		if err := checkpoint(); err != nil {
			if exists {
				st.Cancelled[k] = previous
			} else {
				delete(st.Cancelled, k)
			}
			return err
		}
		return nil
	}
	handled := func(k string, m Moved) error {
		delete(st.Cancelled, k)
		m.At = now
		st.Rerun[k] = m
		return checkpoint()
	}
	fail := func(k string, m Moved, why string) error {
		m.Stage, m.Problem = needsAttention, why
		return save(k, m)
	}
	var errs []error
	readError := func(k string, m Moved, err error) error {
		errs = append(errs, err)
		if now.Sub(m.since()) >= recoveryWait {
			return fail(k, m, "cannot verify recovery: "+err.Error())
		}
		return nil
	}
	for k, m := range st.Cancelled {
		if m.At.IsZero() || m.Attempt < 1 || m.ID < 1 || m.Repo == "" {
			if err := fail(k, m, "incomplete recovery ownership record"); err != nil {
				return out, err
			}
			continue
		}
		run, err := w.run(m.Run)
		if err != nil {
			errs = append(errs, err)
			if now.Sub(m.since()) >= recoveryWait && m.Stage != needsAttention {
				if err := fail(k, m, "cannot verify run: "+err.Error()); err != nil {
					return out, err
				}
			}
			continue
		}
		if m.Stage == needsAttention {
			if run.Attempt != m.Attempt || (run.Status == "completed" && run.Conclusion == "success") {
				if err := handled(k, m); err != nil {
					return out, err
				}
			}
			continue
		}
		if m.Stage == cancelRequested {
			if err := fail(k, m, "normal cancellation outcome is unconfirmed; inspect the run before retrying"); err != nil {
				return out, err
			}
			continue
		}
		if m.Stage != "" && m.Stage != forceRequested && m.Stage != rerunRequested {
			if err := fail(k, m, "unknown recovery stage"); err != nil {
				return out, err
			}
			continue
		}
		if m.Stage == rerunRequested {
			if run.Attempt == m.Attempt {
				if now.Sub(m.since()) >= recoveryWait {
					if err := fail(k, m, "rerun did not create a new attempt"); err != nil {
						return out, err
					}
				}
				continue
			}
			if run.Attempt != m.Attempt+1 {
				if err := handled(k, m); err != nil {
					return out, err
				}
				continue
			}
			next := m.Run
			next.Attempt = run.Attempt
			// An attention record must track the observed rerun, not its cancelled predecessor.
			m.Attempt = run.Attempt
			jobs, err := w.jobs(next)
			if err != nil {
				if err := readError(k, m, err); err != nil {
					return out, err
				}
				continue
			}
			visible, match := false, true
			for _, j := range jobs {
				if j.Status == "completed" && j.Conclusion == "skipped" {
					continue
				}
				if j.ID == 0 || j.Status == "" {
					match = false
				}
				visible = true
				match = match && slices.ContainsFunc(m.Expected, func(label string) bool { return slices.Contains(j.Labels, label) })
			}
			switch {
			case visible && !match:
				if err := fail(k, m, "rerun still targets an unexpected runner label; inspect workflow runs-on"); err != nil {
					return out, err
				}
			case visible:
				if err := handled(k, m); err != nil {
					return out, err
				}
				out.Rerun = append(out.Rerun, k)
			case now.Sub(m.since()) >= recoveryWait:
				if err := fail(k, m, "rerun jobs did not appear; runner label is unverified"); err != nil {
					return out, err
				}
			}
			continue
		}
		if run.Attempt != m.Attempt {
			if err := handled(k, m); err != nil {
				return out, err
			}
			continue
		}
		if run.Status == "completed" && run.Conclusion != "cancelled" {
			if err := handled(k, m); err != nil {
				return out, err
			}
			continue
		}
		if run.Status != "completed" {
			if now.Sub(m.since()) < recoveryWait {
				continue
			}
			if m.Stage == forceRequested {
				if err := fail(k, m, "force cancellation did not complete; GitHub may require manual recovery"); err != nil {
					return out, err
				}
				continue
			}
			if m.Stage != "" {
				if err := fail(k, m, "unknown recovery stage"); err != nil {
					return out, err
				}
				continue
			}
			if run.Status != "queued" && run.Status != "in_progress" {
				if err := fail(k, m, "run is not in a runner queue; inspect concurrency or deployment approvals"); err != nil {
					return out, err
				}
				continue
			}
		}
		if len(targets[m.Repo]) == 0 {
			if err := fail(k, m, "repository has no applied target route"); err != nil {
				return out, err
			}
			continue
		}
		jobs, err := w.jobs(m.Run)
		if err != nil {
			if err := readError(k, m, err); err != nil {
				return out, err
			}
			continue
		}
		if len(jobs) == 0 {
			if err := fail(k, m, "no attempt jobs are visible; cancellation safety cannot be verified"); err != nil {
				return out, err
			}
			continue
		}
		action := "force-cancel"
		if run.Status == "completed" {
			action = "rerun-failed-jobs"
			cancelled := false
			safe := true
			for _, j := range jobs {
				if j.Status == "completed" && j.Conclusion == "cancelled" && unstarted(j) {
					cancelled = true
					continue
				}
				if j.Status != "completed" || (j.Conclusion != "success" && j.Conclusion != "skipped" && j.Conclusion != "neutral") {
					safe = false
				}
			}
			if !safe || !cancelled || len(targets[m.Repo]) == 0 {
				if err := fail(k, m, "cancelled jobs or target labels cannot be safely verified"); err != nil {
					return out, err
				}
				continue
			}
		} else if !slices.ContainsFunc(jobs, func(j recoveryJob) bool { return j.Status == "queued" }) || !allUnstarted(jobs) {
			if err := fail(k, m, "work started or job metadata is ambiguous; refusing force cancellation"); err != nil {
				return out, err
			}
			continue
		}
		// Recheck after paginated job reads, immediately before recording the POST.
		latest, err := w.run(m.Run)
		if err != nil {
			if err := readError(k, m, err); err != nil {
				return out, err
			}
			continue
		}
		if latest.Attempt != m.Attempt {
			if err := handled(k, m); err != nil {
				return out, err
			}
			continue
		}
		if latest.Status != run.Status || latest.Conclusion != run.Conclusion {
			continue
		}
		m.Stage, m.StageAt, m.Problem = forceRequested, now, ""
		if action == "rerun-failed-jobs" {
			m.Stage, m.Expected = rerunRequested, slices.Clone(targets[m.Repo])
		}
		if err := save(k, m); err != nil {
			return out, err
		}
		if err := w.GH.Send(http.MethodPost, runPath(m.Run)+"/"+action, nil); err != nil {
			// Keep the intent, including on ambiguous network failures; never repeat POST.
			if err := fail(k, m, action+" was not confirmed: "+err.Error()); err != nil {
				return out, err
			}
		} else if action == "force-cancel" {
			out.Forced = append(out.Forced, k)
		}
	}
	for k, m := range st.Rerun {
		if now.Sub(m.At) > rerunMemory {
			run, err := w.run(m.Run)
			if err != nil {
				errs = append(errs, err)
			} else if run.Status == "completed" {
				delete(st.Rerun, k)
			}
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
		run, err := w.run(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if run.Attempt != r.Attempt || run.Status == "completed" {
			continue
		}
		jobs, err := w.jobs(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !movable(jobs, r.Label) {
			continue
		}
		latest, err := w.run(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if latest != run {
			continue
		}
		m := Moved{Run: r, At: now, Stage: cancelRequested, StageAt: now}
		if err := save(k, m); err != nil {
			return out, err
		}
		if err := w.GH.Send(http.MethodPost, runPath(r)+"/cancel", nil); err != nil {
			if err := fail(k, m, "normal cancellation was not confirmed: "+err.Error()); err != nil {
				return out, err
			}
			continue
		}
		m.Stage = ""
		if err := save(k, m); err != nil {
			return out, err
		}
		out.Cancelled = append(out.Cancelled, k)
	}
	sort.Strings(out.Cancelled)
	sort.Strings(out.Forced)
	sort.Strings(out.Rerun)
	return out, errors.Join(errs...)
}

func allUnstarted(jobs []recoveryJob) bool {
	for _, j := range jobs {
		if !unstarted(j) {
			return false
		}
	}
	return true
}
