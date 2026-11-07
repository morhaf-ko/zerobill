// Package collect reads live provider state from GitHub for the routing policy.
package collect

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/morhaf-ko/zerobill/internal/github"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

// linuxSKU is the billing usage SKU for GitHub-hosted Linux minutes, the only SKU
// the free minute allowance counts at a 1x rate.
const linuxSKU = "Actions Linux"

// Collector observes providers for one GitHub owner, either an organization or a user.
type Collector struct {
	GH    *github.Client
	Owner string
	Org   bool
	// Repositories, as owner/name, are scanned for queued self-hosted jobs, and for
	// a user owner also for self-hosted runners.
	Repositories []string
	// StatePath caches this month's Blacksmith jobs between runs.
	StatePath string
	Now       func() time.Time
}

// Result holds the state of every provider that was observed and the error of
// every provider that was not.
type Result struct {
	States map[string]policy.State
	Errors map[string]string
	// Movable lists, per self-hosted provider, runs with a job queued on its label
	// that can be cancelled and rerun without stopping a started job.
	Movable map[string][]Run
}

// Run identifies one attempt of a workflow run.
type Run struct {
	Repo    string `json:"repo"`
	ID      int64  `json:"id"`
	Attempt int    `json:"attempt"`
}

// Observe collects state for each provider. A failed provider is reported in
// Result.Errors and left out of Result.States, so the policy skips it.
func (c *Collector) Observe(providers []policy.Provider) Result {
	r := Result{States: map[string]policy.State{}, Errors: map[string]string{}, Movable: map[string][]Run{}}
	for _, p := range providers {
		var s policy.State
		var err error
		switch p.Name {
		case "runner":
			var movable []Run
			s, movable, err = c.selfHosted(p.Label)
			if len(movable) > 0 {
				r.Movable[p.Name] = movable
			}
		case "blacksmith":
			var u *usage
			if u, err = c.monthUsage(); err == nil {
				s.UsedMinutes = u.blacksmith()
			}
		case "github":
			s, err = c.gitHubHosted()
		default:
			err = fmt.Errorf("no collector for provider %q", p.Name)
		}
		if err != nil {
			r.Errors[p.Name] = err.Error()
			continue
		}
		r.States[p.Name] = s
	}
	return r
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

// IsOrg reports whether owner is an organization rather than a user.
func IsOrg(gh *github.Client, owner string) (bool, error) {
	var u struct {
		Type string `json:"type"`
	}
	if err := gh.Get("/users/"+url.PathEscape(owner), &u); err != nil {
		return false, err
	}
	return u.Type == "Organization", nil
}

type runner struct {
	Status string        `json:"status"`
	Labels []runnerLabel `json:"labels"`
}

type runnerLabel struct {
	Name string `json:"name"`
}

type workflowRun struct {
	ID         int64  `json:"id"`
	RunAttempt int    `json:"run_attempt"`
	Status     string `json:"status"`
}

type job struct {
	ID          int64      `json:"id"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	Labels      []string   `json:"labels"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	// RunnerName is empty for a job that never reached a runner, such as one
	// cancelled while queued; its started_at still records queue time.
	RunnerName string `json:"runner_name"`
}

// selfHosted reads whether runners carry label and are online, and the age of the
// oldest job queued on it. It also returns the runs that can move off the label:
// every queued job is on it, no job has started, and no completed job failed or
// was cancelled, so rerunning failed jobs reruns only what zerobill cancelled.
func (c *Collector) selfHosted(label string) (policy.State, []Run, error) {
	paths := []string{"/orgs/" + url.PathEscape(c.Owner) + "/actions/runners?per_page=100"}
	if !c.Org {
		paths = paths[:0]
		for _, repo := range c.Repositories {
			paths = append(paths, "/repos/"+repo+"/actions/runners?per_page=100")
		}
	}
	var s policy.State
	for _, path := range paths {
		err := c.GH.Paginate(path, func(body []byte) error {
			var page struct {
				Runners []runner `json:"runners"`
			}
			if err := json.Unmarshal(body, &page); err != nil {
				return err
			}
			for _, r := range page.Runners {
				if slices.ContainsFunc(r.Labels, func(l runnerLabel) bool { return l.Name == label }) {
					s.Registered = true
					s.Online = s.Online || r.Status == "online"
				}
			}
			return nil
		})
		if err != nil {
			return policy.State{}, nil, err
		}
	}

	var movable []Run
	now := c.now()
	for _, repo := range c.Repositories {
		seen := map[int64]bool{}
		// Later jobs of a running workflow queue too, so in-progress runs are scanned.
		// Runs that are waiting, requested or pending hold no runner-queued jobs.
		for _, status := range []string{"queued", "in_progress"} {
			runs, err := c.runs("/repos/" + repo + "/actions/runs?status=" + status + "&per_page=100")
			if err != nil {
				return policy.State{}, nil, err
			}
			for _, run := range runs {
				if seen[run.ID] {
					continue
				}
				seen[run.ID] = true
				jobs, err := c.jobs(repo, run.ID, "latest")
				if err != nil {
					return policy.State{}, nil, err
				}
				onLabel, pinned := false, false
				for _, j := range jobs {
					switch {
					case j.Status == "queued" && slices.Contains(j.Labels, label):
						onLabel = true
						if age := now.Sub(j.CreatedAt); age > s.OldestQueued {
							s.OldestQueued = age
						}
					case j.Status == "completed" && (j.Conclusion == "success" || j.Conclusion == "skipped" || j.Conclusion == "neutral"):
					default:
						pinned = true
					}
				}
				if onLabel && !pinned {
					movable = append(movable, Run{Repo: repo, ID: run.ID, Attempt: run.RunAttempt})
				}
			}
		}
	}
	return s, movable, nil
}

// gitHubHosted sums this month's GitHub-hosted Linux minutes from the billing
// usage report.
func (c *Collector) gitHubHosted() (policy.State, error) {
	now := c.now()
	base := "/users/"
	if c.Org {
		base = "/organizations/"
	}
	path := fmt.Sprintf("%s%s/settings/billing/usage?year=%d&month=%d", base, url.PathEscape(c.Owner), now.Year(), int(now.Month()))
	var report struct {
		UsageItems []struct {
			Product  string  `json:"product"`
			SKU      string  `json:"sku"`
			Quantity float64 `json:"quantity"`
			UnitType string  `json:"unitType"`
		} `json:"usageItems"`
	}
	if err := c.GH.Get(path, &report); err != nil {
		return policy.State{}, err
	}
	var s policy.State
	for _, it := range report.UsageItems {
		if strings.EqualFold(it.Product, "actions") && it.SKU == linuxSKU && it.UnitType == "Minutes" {
			s.UsedMinutes += it.Quantity
		}
	}
	return s, nil
}

var blacksmithVCPU = regexp.MustCompile(`^blacksmith-(\d+)vcpu-`)

// isBlacksmith reports whether a runs-on label targets a Blacksmith runner.
func isBlacksmith(label string) bool {
	return strings.HasPrefix(label, "blacksmith-")
}

// blacksmithMultiplier converts wall-clock minutes on a label into billed
// 2-vCPU minutes. Labels without a vCPU count bill as 2 vCPU.
func blacksmithMultiplier(label string) float64 {
	m := blacksmithVCPU.FindStringSubmatch(label)
	if m == nil {
		return 1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 1
	}
	return float64(n) / 2
}

// ranMinutes is a job's wall-clock duration rounded up to whole minutes, zero
// for a job that never reached a runner.
func ranMinutes(j job, end time.Time) float64 {
	if j.RunnerName == "" || j.StartedAt == nil {
		return 0
	}
	d := end.Sub(*j.StartedAt)
	if d <= 0 {
		return 0
	}
	return math.Ceil(d.Minutes())
}

// usage holds this month's jobs that ran on a runner.
type usage struct {
	done    map[string]jobRecord
	running []jobRecord
}

// blacksmith sums billed Blacksmith minutes, scaling each job by vCPU/2.
func (u *usage) blacksmith() float64 {
	var total float64
	add := func(r jobRecord) {
		for _, l := range r.Labels {
			if isBlacksmith(l) {
				total += r.Minutes * blacksmithMultiplier(l)
				return
			}
		}
	}
	for _, r := range u.done {
		add(r)
	}
	for _, r := range u.running {
		add(r)
	}
	return total
}

// monthUsage reads the jobs of runs created this month in every repository the
// owner has, caching completed jobs in StatePath.
func (c *Collector) monthUsage() (*usage, error) {
	now := c.now()
	st, err := loadState(c.StatePath, now.Format("2006-01"))
	if err != nil {
		return nil, err
	}
	repos, err := c.GH.Repos(c.Owner, c.Org)
	if err != nil {
		return nil, err
	}
	u := &usage{done: st.Jobs}
	for _, r := range repos {
		repo := r.Name
		runs, err := c.monthRuns(repo, now)
		if err != nil {
			return nil, err
		}
		for _, run := range runs {
			key := fmt.Sprintf("%d/%d", run.ID, run.RunAttempt)
			if st.Runs[key] {
				continue
			}
			jobs, err := c.jobs(repo, run.ID, "all")
			if err != nil {
				return nil, err
			}
			done := run.Status == "completed"
			for _, j := range jobs {
				id := strconv.FormatInt(j.ID, 10)
				if _, ok := st.Jobs[id]; ok {
					continue
				}
				switch {
				case j.Status == "completed" && j.CompletedAt != nil:
					st.Jobs[id] = jobRecord{Labels: j.Labels, Minutes: ranMinutes(j, *j.CompletedAt)}
				case j.Status == "completed":
					st.Jobs[id] = jobRecord{Labels: j.Labels}
				default:
					done = false
					u.running = append(u.running, jobRecord{Labels: j.Labels, Minutes: ranMinutes(j, now)})
				}
			}
			if done {
				st.Runs[key] = true
			}
		}
	}
	if err := st.save(c.StatePath); err != nil {
		return nil, err
	}
	return u, nil
}

// runSearchCap is the most runs the API returns for a query filtered by created or status.
const runSearchCap = 1000

// monthRuns lists runs created this month (UTC), one query per day when the month
// exceeds the search cap.
func (c *Collector) monthRuns(repo string, now time.Time) ([]workflowRun, error) {
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	runs, total, err := c.runsTotal("/repos/" + repo + "/actions/runs?per_page=100&created=" + url.QueryEscape(">="+start.Format("2006-01-02")))
	if err != nil || total <= runSearchCap {
		return runs, err
	}
	runs = runs[:0]
	for day := start; !day.After(now); day = day.AddDate(0, 0, 1) {
		page, total, err := c.runsTotal("/repos/" + repo + "/actions/runs?per_page=100&created=" + day.Format("2006-01-02"))
		if err != nil {
			return nil, err
		}
		if total > runSearchCap {
			return nil, fmt.Errorf("%s: %d runs on %s exceed the API's %d-result cap", repo, total, day.Format("2006-01-02"), runSearchCap)
		}
		runs = append(runs, page...)
	}
	return runs, nil
}

func (c *Collector) runs(path string) ([]workflowRun, error) {
	runs, total, err := c.runsTotal(path)
	if err == nil && total > runSearchCap {
		return nil, fmt.Errorf("%s: %d runs exceed the API's %d-result cap", path, total, runSearchCap)
	}
	return runs, err
}

func (c *Collector) runsTotal(path string) ([]workflowRun, int, error) {
	var runs []workflowRun
	var total int
	err := c.GH.Paginate(path, func(body []byte) error {
		var page struct {
			TotalCount   int           `json:"total_count"`
			WorkflowRuns []workflowRun `json:"workflow_runs"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		total = page.TotalCount
		if total > runSearchCap {
			// Callers reject or split a capped query, so later pages are wasted requests.
			return github.ErrStop
		}
		runs = append(runs, page.WorkflowRuns...)
		return nil
	})
	return runs, total, err
}

func (c *Collector) jobs(repo string, runID int64, filter string) ([]job, error) {
	var jobs []job
	path := fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=%s&per_page=100", repo, runID, filter)
	err := c.GH.Paginate(path, func(body []byte) error {
		var page struct {
			Jobs []job `json:"jobs"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		jobs = append(jobs, page.Jobs...)
		return nil
	})
	return jobs, err
}
