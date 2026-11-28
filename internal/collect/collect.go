// Package collect reads live provider state from GitHub for the routing policy.
package collect

import (
	"encoding/json"
	"errors"
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
	// Inventory can share a check's owner repository listing with discovery.
	Inventory func() ([]github.Repo, error)
}

// Result holds the state of every provider that was observed and the error of
// every provider that was not.
type Result struct {
	States        map[string]policy.State
	Errors        map[string]string
	AccessProblem bool
	RateLimited   bool
	// Movable lists, per self-hosted provider, runs with a job queued on its label
	// that can be cancelled and rerun without stopping a started job.
	Movable map[string][]Run
	Queues  map[string]QueueDiagnostics
}

type QueueDiagnostics struct {
	Pool   string         `json:"pool"`
	Causes map[string]int `json:"causes,omitempty"`
}

// Run identifies one attempt of a workflow run.
type Run struct {
	Repo    string `json:"repo"`
	ID      int64  `json:"id"`
	Attempt int    `json:"attempt"`
	Label   string `json:"label,omitempty"`
}

// Observe collects state for each provider. A failed provider is reported in
// Result.Errors and left out of Result.States, so the policy skips it.
func (c *Collector) Observe(providers []policy.Provider) Result {
	r := Result{States: map[string]policy.State{}, Errors: map[string]string{}, Movable: map[string][]Run{}, Queues: map[string]QueueDiagnostics{}}
	month := c.now().Format("2006-01")
	for _, p := range providers {
		var s policy.State
		var err error
		switch p.Name {
		case "runner":
			var movable []Run
			var diagnostic QueueDiagnostics
			s, movable, diagnostic, err = c.selfHosted(p.Label, p.QueueTimeout)
			if err == nil {
				r.Queues[p.Name] = diagnostic
			}
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
		if err == nil && p.Kind == policy.Metered && c.now().Format("2006-01") != month {
			err = fmt.Errorf("billing month changed during collection")
		}
		if err != nil {
			r.Errors[p.Name] = err.Error()
			r.AccessProblem = r.AccessProblem || github.IsAccessDenied(err)
			r.RateLimited = r.RateLimited || github.IsRateLimited(err)
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
	Busy   bool          `json:"busy"`
	Labels []runnerLabel `json:"labels"`
}

type runnerLabel struct {
	Name string `json:"name"`
}

type workflowRun struct {
	ID         int64     `json:"id"`
	RunAttempt int       `json:"run_attempt"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
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
	RunnerID   int64  `json:"runner_id"`
	GroupID    int64  `json:"runner_group_id"`
	RunAttempt int    `json:"run_attempt"`
}

// selfHosted reads whether runners carry label and are online, and the age of the
// oldest job queued on it. It also returns the runs that can move off the label:
// every queued job is on it, no job is running, and no completed job failed or
// was cancelled, so rerunning failed jobs reruns only what zerobill cancelled.
func (c *Collector) selfHosted(label string, timeout time.Duration) (policy.State, []Run, QueueDiagnostics, error) {
	if timeout == 0 {
		timeout = policy.MaxQueueWait
	}
	owners := c.Repositories
	base := "/repos/"
	if c.Org {
		owners, base = []string{url.PathEscape(c.Owner)}, "/orgs/"
	}
	inventory := map[string][]runner{}
	var s policy.State
	for _, owner := range owners {
		err := c.GH.ConditionalPaginate(base+owner+"/actions/runners?per_page=100", func(body []byte) error {
			var page struct {
				Runners []runner `json:"runners"`
			}
			if err := json.Unmarshal(body, &page); err != nil {
				return err
			}
			for _, r := range page.Runners {
				if hasLabel(r, label) {
					inventory[owner] = append(inventory[owner], r)
					s.Registered = true
					s.Online = s.Online || r.Status == "online"
					s.Idle = s.Idle || (r.Status == "online" && !r.Busy)
				}
			}
			return nil
		})
		if err != nil {
			return policy.State{}, nil, QueueDiagnostics{}, err
		}
	}

	diagnostic := QueueDiagnostics{Causes: map[string]int{}}
	switch {
	case s.Idle:
		diagnostic.Pool = "matching runner online and idle"
	case s.Online:
		diagnostic.Pool = "matching online runners busy"
	case s.Registered:
		diagnostic.Pool = "matching runners registered but none online"
	default:
		diagnostic.Pool = "no matching runner registered"
	}

	var movable []Run
	now := c.now()
	for _, repo := range c.Repositories {
		runners := inventory[repo]
		if c.Org {
			runners = inventory[url.PathEscape(c.Owner)]
		}
		seen := map[int64]bool{}
		for _, status := range []string{"queued", "in_progress"} {
			runs, err := c.runs("/repos/" + repo + "/actions/runs?status=" + status + "&per_page=100")
			if err != nil {
				return policy.State{}, nil, QueueDiagnostics{}, err
			}
			for _, run := range runs {
				if seen[run.ID] {
					continue
				}
				seen[run.ID] = true
				if run.Status != "queued" && run.Status != "in_progress" {
					diagnostic.Causes["run left runner-queue view; GitHub approvals, concurrency or other state possible"]++
					continue
				}
				jobs, err := c.jobs(repo, run.ID, "latest")
				if err != nil {
					return policy.State{}, nil, QueueDiagnostics{}, err
				}
				onLabel, pinned := false, false
				for _, j := range jobs {
					switch {
					case j.Status == "queued" && slices.Contains(j.Labels, label):
						if j.RunnerName != "" || j.RunnerID != 0 {
							diagnostic.Causes["runner assigned; execution unverified"]++
							pinned = true
							continue
						}
						if j.GroupID != 0 || c.Org {
							diagnostic.Causes["runner group access unverified"]++
						}
						age, issue := queueAge(j, run, now)
						if issue != "" {
							diagnostic.Causes[issue]++
							pinned = true
						} else {
							onLabel = true
							s.OldestQueued = max(s.OldestQueued, age)
						}
						diagnostic.Causes[queueCause(runners, j.Labels, age, timeout)]++
					case j.Status == "completed" && (j.Conclusion == "success" || j.Conclusion == "skipped" || j.Conclusion == "neutral"):
					case j.Status == "waiting" || j.Status == "pending" || j.Status == "requested":
						diagnostic.Causes["job waiting on GitHub; inspect approvals or concurrency"]++
						pinned = true
					default:
						pinned = true
					}
				}
				if onLabel && !pinned {
					movable = append(movable, Run{Repo: repo, ID: run.ID, Attempt: run.RunAttempt, Label: label})
				}
			}
		}
	}
	return s, movable, diagnostic, nil
}

func hasLabel(r runner, label string) bool {
	return slices.ContainsFunc(r.Labels, func(l runnerLabel) bool { return l.Name == label })
}

func queueAge(j job, run workflowRun, now time.Time) (time.Duration, string) {
	switch {
	case j.RunAttempt != 0 && j.RunAttempt != run.RunAttempt:
		return 0, "queue observation is stale; job attempt changed"
	case j.CreatedAt.IsZero():
		return 0, "queue age unknown; job creation time missing"
	case j.CreatedAt.After(now):
		return 0, "queue age unknown; job creation time is in the future"
	case j.CreatedAt.Before(run.CreatedAt):
		return 0, "queue age unknown; job creation time predates the run"
	default:
		return now.Sub(j.CreatedAt), ""
	}
}

func queueCause(runners []runner, labels []string, age, timeout time.Duration) string {
	if len(runners) == 0 {
		switch {
		case age > timeout:
			return "queue timeout with no registered runner; cold start or pool outage"
		case age > 0:
			return "no registered runner yet; cold start possible"
		default:
			return "no registered runner; cause unknown"
		}
	}
	matches, online := false, false
	for _, r := range runners {
		if !slices.ContainsFunc(labels, func(label string) bool { return !hasLabel(r, label) }) {
			matches = true
			if r.Status == "online" {
				online = true
				if !r.Busy {
					return "idle runner has all labels; queue cause unknown"
				}
			}
		}
	}
	switch {
	case online:
		return "online runners with all labels are busy"
	case matches:
		return "runners with all labels are registered but none online"
	default:
		return "no visible runner has all extra labels"
	}
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
	if report.UsageItems == nil {
		return policy.State{}, fmt.Errorf("billing report is missing usage items")
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
func (c *Collector) monthUsage() (_ *usage, resultErr error) {
	now := c.now()
	st, err := loadState(c.StatePath, now.Format("2006-01"))
	if err != nil {
		return nil, err
	}
	inventory := c.Inventory
	if inventory == nil {
		inventory = func() ([]github.Repo, error) { return c.GH.Repos(c.Owner, c.Org) }
	}
	repos, err := inventory()
	if err != nil {
		return nil, err
	}
	private := 0
	for _, repo := range repos {
		if repo.Private {
			private++
		}
	}
	if private == 0 {
		return nil, fmt.Errorf("no private repositories visible; Blacksmith usage is unknown")
	}
	u := &usage{done: st.Jobs}
	changed := false
	defer func() {
		if changed {
			resultErr = errors.Join(resultErr, st.save(c.StatePath))
		}
	}()
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
					changed = true
					st.Jobs[id] = jobRecord{Labels: j.Labels, Minutes: ranMinutes(j, *j.CompletedAt)}
				case j.Status == "completed":
					changed = true
					st.Jobs[id] = jobRecord{Labels: j.Labels}
				default:
					done = false
					u.running = append(u.running, jobRecord{Labels: j.Labels, Minutes: ranMinutes(j, now)})
				}
			}
			if done {
				changed = true
				st.Runs[key] = true
			}
		}
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
	err := c.GH.ConditionalPaginate(path, func(body []byte) error {
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
	err := c.GH.ConditionalPaginate(path, func(body []byte) error {
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
