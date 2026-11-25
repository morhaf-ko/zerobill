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
	"github.com/morhaf-ko/zerobill/internal/config"
	"github.com/morhaf-ko/zerobill/internal/discover"
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
	Recovery                 map[string]string
}

// MigratedJob records the original job ID and the stable name used across attempts.
type MigratedJob struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Variable string `json:"variable"`
	Label    string `json:"label"`
}

type remoteRun struct {
	Path       string `json:"path"`
	SHA        string `json:"head_sha"`
	Event      string `json:"event"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Attempt    int    `json:"run_attempt"`
}

type recoveryJob struct {
	Name       string         `json:"name"`
	ID         int64          `json:"id"`
	Status     string         `json:"status"`
	Conclusion string         `json:"conclusion"`
	Labels     []string       `json:"labels"`
	Runner     string         `json:"runner_name"`
	RunAttempt int            `json:"run_attempt"`
	RunnerID   int64          `json:"runner_id"`
	Steps      []recoveryStep `json:"steps"`
}

type recoveryStep struct {
	Status    string     `json:"status"`
	StartedAt *time.Time `json:"started_at"`
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
	total := -1
	err := w.GH.Paginate(fmt.Sprintf("%s/attempts/%d/jobs?per_page=100", runPath(r), r.Attempt), func(raw []byte) error {
		var page struct {
			Total *int          `json:"total_count"`
			Jobs  []recoveryJob `json:"jobs"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		if page.Total == nil || (total >= 0 && total != *page.Total) {
			return fmt.Errorf("attempt job count is missing or changed across pages")
		}
		total = *page.Total
		for _, j := range page.Jobs {
			if j.RunAttempt != 0 && j.RunAttempt != r.Attempt {
				return fmt.Errorf("job %d belongs to attempt %d, expected %d", j.ID, j.RunAttempt, r.Attempt)
			}
		}
		jobs = append(jobs, page.Jobs...)
		return nil
	})
	if err == nil && len(jobs) != total {
		err = fmt.Errorf("attempt reports %d jobs but returned %d", total, len(jobs))
	}
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

func runnerQueue(status string) bool {
	return status == "queued" || status == "in_progress"
}

func migrationJobs(jobs []recoveryJob, source string, cancelled bool) ([]recoveryJob, error) {
	var affected []recoveryJob
	names := map[string]bool{}
	for _, j := range jobs {
		if j.Name == "" || names[j.Name] || j.ID <= 0 {
			return nil, fmt.Errorf("attempt job identities are incomplete or ambiguous")
		}
		names[j.Name] = true
		switch {
		case j.Status == "completed" && (j.Conclusion == "success" || j.Conclusion == "neutral" || j.Conclusion == "skipped"):
			continue
		case !cancelled && j.Status == "queued" && unstarted(j):
		case cancelled && j.Status == "completed" && j.Conclusion == "cancelled" && unstarted(j):
		default:
			return nil, fmt.Errorf("job %q has status %s/%s or has reached a runner", j.Name, j.Status, j.Conclusion)
		}
		if source == "" || len(j.Labels) != 1 || j.Labels[0] != source {
			return nil, fmt.Errorf("job %q does not have the single stalled label %q", j.Name, source)
		}
		affected = append(affected, j)
	}
	if len(affected) == 0 {
		return nil, fmt.Errorf("no affected jobs are visible in the attempt")
	}
	return affected, nil
}

// Targets maps successfully applied variables to eligible labels. An empty label means
// the route was evaluated and is ineligible; an absent variable has no fresh evidence.
type Targets map[string]map[string]string

var errUnverifiedRoute = errors.New("replacement route could not be verified this check")

func (w *Writer) migration(r collect.Run, run remoteRun, affected []recoveryJob, targets map[string]string, previous []MigratedJob) ([]MigratedJob, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("%w for %s", errUnverifiedRoute, r.Repo)
	}
	for _, job := range previous {
		if _, verified := targets[job.Variable]; !verified {
			return nil, fmt.Errorf("%w for %s", errUnverifiedRoute, job.Variable)
		}
	}
	routes, err := discover.JobRoutes(w.GH, r.Repo, run.Path, run.SHA, run.Event)
	if err != nil {
		return nil, err
	}
	plan := make([]MigratedJob, 0, len(affected))
	for _, j := range affected {
		route := routes[j.Name]
		if route.Dependent != "" {
			return nil, fmt.Errorf("job %q depends on %q and GitHub could rerun it", route.Dependent, j.Name)
		}
		variable := route.Variable
		label, verified := targets[variable]
		if variable != "" && !verified {
			return nil, fmt.Errorf("%w for %s", errUnverifiedRoute, variable)
		}
		if variable == "" || label == "" || label == r.Label || label == config.WaitingLabel {
			return nil, fmt.Errorf("job %q has no eligible replacement for its runs-on declaration", j.Name)
		}
		plan = append(plan, MigratedJob{ID: j.ID, Name: j.Name, Variable: variable, Label: label})
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].ID < plan[j].ID })
	if previous != nil && !slices.Equal(previous, plan) {
		return nil, fmt.Errorf("affected jobs or their replacement route changed after cancellation")
	}
	checked := map[string]bool{}
	for _, job := range plan {
		if checked[job.Variable] {
			continue
		}
		value, err := w.variable(r.Repo, job.Variable)
		if err != nil {
			return nil, fmt.Errorf("%w for %s: %v", errUnverifiedRoute, job.Variable, err)
		}
		if value != job.Label {
			return nil, fmt.Errorf("%w: %s currently holds %q, expected %q", errUnverifiedRoute, job.Variable, value, job.Label)
		}
		checked[job.Variable] = true
	}
	return plan, nil
}

func matchesPlan(jobs []recoveryJob, r collect.Run, cancelled bool, plan []MigratedJob) error {
	affected, err := migrationJobs(jobs, r.Label, cancelled)
	if err != nil {
		return err
	}
	sort.Slice(affected, func(i, j int) bool { return affected[i].ID < affected[j].ID })
	if !slices.EqualFunc(plan, affected, func(recorded MigratedJob, live recoveryJob) bool {
		return recorded.ID == live.ID && recorded.Name == live.Name
	}) {
		return fmt.Errorf("affected jobs changed during replacement verification")
	}
	return nil
}

type pickup int

const (
	queued pickup = iota
	githubWaiting
	assigned
	started
)

func rerunEvidence(jobs []recoveryJob, expected []MigratedJob, runStatus string) (progress string, confirmed, executing bool, problem string) {
	if len(expected) == 0 {
		return "", false, false, "saved rerun has no migrated job identities; inspect the new attempt"
	}
	visible := map[string]recoveryJob{}
	duplicate := map[string]bool{}
	for _, j := range jobs {
		if _, exists := visible[j.Name]; exists {
			duplicate[j.Name] = true
		}
		visible[j.Name] = j
	}
	least := started
	for _, want := range expected {
		j, found := visible[want.Name]
		if duplicate[want.Name] {
			return "", false, false, fmt.Sprintf("rerun job %q has an ambiguous identity", want.Name)
		}
		if !found || j.ID <= 0 || j.ID == want.ID || j.Status == "" {
			return fmt.Sprintf("waiting for migrated job %q in the new attempt", want.Name), false, false, ""
		}
		if j.Status == "completed" && (j.Conclusion == "skipped" || j.Conclusion == "cancelled") {
			return "", false, false, fmt.Sprintf("migrated job %q was %s; pickup is unverified", want.Name, j.Conclusion)
		}
		if len(j.Labels) == 0 {
			return fmt.Sprintf("waiting for runner labels on migrated job %q", want.Name), false, false, ""
		}
		if len(j.Labels) != 1 || j.Labels[0] != want.Label {
			return "", false, false, fmt.Sprintf("migrated job %q has unexpected runner labels %v; expected %q", want.Name, j.Labels, want.Label)
		}
		switch {
		case j.Status == "in_progress" || slices.ContainsFunc(j.Steps, func(step recoveryStep) bool { return step.StartedAt != nil }):
		case j.Status == "completed" && (j.Runner != "" || j.RunnerID != 0):
		case j.Status == "waiting" || j.Status == "pending" || j.Status == "requested":
			least = min(least, githubWaiting)
		case j.Runner != "" || j.RunnerID != 0:
			least = min(least, assigned)
		default:
			least = queued
		}
	}
	switch {
	case least == started:
		return "replacement labels confirmed; migrated jobs started", true, true, ""
	case runStatus == "completed":
		return "replacement labels confirmed; terminal attempt without verified pickup", true, false, ""
	case least == githubWaiting || runStatus == "waiting" || runStatus == "pending" || runStatus == "requested":
		return "replacement labels confirmed; waiting on GitHub, inspect concurrency or approvals", true, false, ""
	case least == assigned:
		return "replacement labels confirmed; runner assigned, execution unverified", true, false, ""
	default:
		return "replacement labels confirmed; waiting for runner pickup", true, false, ""
	}
}

func (m Moved) since() time.Time {
	if !m.StageAt.IsZero() {
		return m.StageAt
	}
	return m.At
}

func (m Moved) NeedsAttention() bool { return m.Stage == needsAttention }

func (m Moved) PendingStatus() string {
	switch m.Stage {
	case "":
		return "waiting for normal cancellation"
	case forceRequested:
		return "waiting for force cancellation"
	case rerunRequested:
		return "verifying rerun"
	default:
		return m.Stage
	}
}

// Move checkpoints mutation intent before each POST. An uncertain POST is reconciled,
// never resubmitted; GitHub offers no atomic attempt condition on these endpoints.
func (w *Writer) Move(st *State, stuck []collect.Run, targets Targets, now time.Time, checkpoint func() error) (Moves, error) {
	out := Moves{Recovery: map[string]string{}}
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
		previous := st.Cancelled[k]
		delete(st.Cancelled, k)
		m.At = now
		st.Rerun[k] = m
		if err := checkpoint(); err != nil {
			delete(st.Rerun, k)
			st.Cancelled[k] = previous
			return err
		}
		return nil
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
			jobs, err := w.jobs(next)
			if err != nil {
				m.Attempt = run.Attempt
				if err := readError(k, m, err); err != nil {
					return out, err
				}
				continue
			}
			progress, confirmed, executing, problem := rerunEvidence(jobs, m.Jobs, run.Status)
			firstConfirmation := confirmed && !m.LabelsConfirmed
			m.LabelsConfirmed = m.LabelsConfirmed || confirmed
			out.Recovery[k] = fmt.Sprintf("attempt %d: %s", run.Attempt, progress)
			switch {
			case problem != "":
				m.Attempt = run.Attempt
				if err := fail(k, m, problem); err != nil {
					return out, err
				}
			case executing:
				m.Attempt = run.Attempt
				if err := handled(k, m); err != nil {
					return out, err
				}
			case run.Status == "completed" || now.Sub(m.since()) >= recoveryWait:
				m.Attempt = run.Attempt
				if err := fail(k, m, "cannot verify rerun pickup: "+progress); err != nil {
					return out, err
				}
			case firstConfirmation:
				if err := save(k, m); err != nil {
					return out, err
				}
			}
			if firstConfirmation {
				out.Rerun = append(out.Rerun, k)
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
		jobs, err := w.jobs(m.Run)
		if err != nil {
			if err := readError(k, m, err); err != nil {
				return out, err
			}
			continue
		}
		cancelled := run.Status == "completed"
		action := "force-cancel"
		if cancelled {
			action = "rerun-failed-jobs"
		}
		affected, err := migrationJobs(jobs, m.Label, cancelled)
		if err == nil && len(m.Jobs) == 0 {
			err = fmt.Errorf("saved cancellation has no job and replacement route evidence")
		}
		if err == nil {
			_, err = w.migration(m.Run, run, affected, targets[m.Repo], m.Jobs)
		}
		if errors.Is(err, errUnverifiedRoute) {
			out.Recovery[k] = action + " delayed: " + err.Error()
			continue
		}
		if err != nil {
			if err := fail(k, m, action+" skipped: "+err.Error()); err != nil {
				return out, err
			}
			continue
		}
		jobs, err = w.jobs(m.Run)
		if err != nil {
			if err := readError(k, m, err); err != nil {
				return out, err
			}
			continue
		}
		if err := matchesPlan(jobs, m.Run, cancelled, m.Jobs); err != nil {
			if err := fail(k, m, action+" skipped: "+err.Error()); err != nil {
				return out, err
			}
			continue
		}
		// The final run check also catches scheduler or attempt changes during the job reads.
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
		if latest != run {
			out.Recovery[k] = action + " delayed: run changed during verification"
			continue
		}
		m.Stage, m.StageAt, m.Problem = forceRequested, now, ""
		if cancelled {
			m.Stage = rerunRequested
		}
		if err := save(k, m); err != nil {
			return out, err
		}
		if err := w.GH.Send(http.MethodPost, runPath(m.Run)+"/"+action, nil); err != nil {
			// Keep the intent, including on ambiguous network failures; never repeat POST.
			if err := fail(k, m, action+" was not confirmed: "+err.Error()); err != nil {
				return out, err
			}
		} else if !cancelled {
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
		if run.Attempt != r.Attempt || !runnerQueue(run.Status) {
			out.Recovery[k] = fmt.Sprintf("cancel skipped: attempt changed or run status %q is outside the runner queue", run.Status)
			continue
		}
		jobs, err := w.jobs(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		affected, err := migrationJobs(jobs, r.Label, false)
		if err != nil {
			out.Recovery[k] = "cancel skipped: " + err.Error()
			continue
		}
		plan, err := w.migration(r, run, affected, targets[r.Repo], nil)
		if err != nil {
			out.Recovery[k] = "cancel skipped: " + err.Error()
			continue
		}
		jobs, err = w.jobs(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := matchesPlan(jobs, r, false, plan); err != nil {
			out.Recovery[k] = "cancel skipped: " + err.Error()
			continue
		}
		latest, err := w.run(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if latest != run {
			out.Recovery[k] = "cancel skipped: run changed during verification"
			continue
		}
		m := Moved{Run: r, At: now, Stage: cancelRequested, StageAt: now, Jobs: plan}
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
