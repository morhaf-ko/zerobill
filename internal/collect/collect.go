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
	GH *github.Client
	// Owner owns the runners, billing account and repositories that are observed.
	Owner string
	// Repositories, as owner/name, are scanned for queued self-hosted jobs, and for
	// a user owner also for self-hosted runners.
	Repositories []string
	// StatePath caches Blacksmith job minutes between runs; empty disables the cache.
	StatePath string
	Now       func() time.Time

	isOrg *bool
}

// Result holds the state of every provider that was observed and the error of
// every provider that was not.
type Result struct {
	States map[string]policy.State
	Errors map[string]string
}

// Observe collects state for each provider. A failed provider is reported in
// Result.Errors and left out of Result.States, so the policy skips it.
func (c *Collector) Observe(providers []policy.Provider) Result {
	r := Result{States: map[string]policy.State{}, Errors: map[string]string{}}
	for _, p := range providers {
		var s policy.State
		var err error
		switch {
		case p.Kind == policy.SelfHosted:
			s, err = c.SelfHosted(p.Label)
		case p.Kind == policy.Metered && isBlacksmith(p.Label):
			s, err = c.Blacksmith()
		case p.Kind == policy.Metered && strings.HasPrefix(p.Label, "ubuntu-"):
			s, err = c.GitHubHosted()
		case p.Kind == policy.Metered:
			err = fmt.Errorf("no collector for metered label %q; use blacksmith-* or ubuntu-*", p.Label)
		default:
			err = fmt.Errorf("unknown kind %q", p.Kind)
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

func (c *Collector) ownerIsOrg() (bool, error) {
	if c.isOrg != nil {
		return *c.isOrg, nil
	}
	var u struct {
		Type string `json:"type"`
	}
	if err := c.GH.Get("/users/"+url.PathEscape(c.Owner), &u); err != nil {
		return false, err
	}
	org := u.Type == "Organization"
	c.isOrg = &org
	return org, nil
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
	Labels      []string   `json:"labels"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

// SelfHosted reports a label online when any runner carrying it is online, and
// the age of the oldest queued job targeting it in the configured repositories.
func (c *Collector) SelfHosted(label string) (policy.State, error) {
	org, err := c.ownerIsOrg()
	if err != nil {
		return policy.State{}, err
	}
	paths := []string{"/orgs/" + url.PathEscape(c.Owner) + "/actions/runners?per_page=100"}
	if !org {
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
				if r.Status == "online" && slices.ContainsFunc(r.Labels, func(l runnerLabel) bool { return l.Name == label }) {
					s.Online = true
				}
			}
			return nil
		})
		if err != nil {
			return policy.State{}, err
		}
	}

	now := c.now()
	for _, repo := range c.Repositories {
		seen := map[int64]bool{}
		// Later jobs of a running workflow queue too, so in-progress runs are scanned.
		// Runs that are waiting, requested or pending hold no runner-queued jobs.
		for _, status := range []string{"queued", "in_progress"} {
			runs, err := c.runs("/repos/" + repo + "/actions/runs?status=" + status + "&per_page=100")
			if err != nil {
				return policy.State{}, err
			}
			for _, run := range runs {
				if seen[run.ID] {
					continue
				}
				seen[run.ID] = true
				jobs, err := c.jobs(repo, run.ID, "latest")
				if err != nil {
					return policy.State{}, err
				}
				for _, j := range jobs {
					if j.Status == "queued" && slices.Contains(j.Labels, label) {
						if age := now.Sub(j.CreatedAt); age > s.OldestQueued {
							s.OldestQueued = age
						}
					}
				}
			}
		}
	}
	return s, nil
}

// GitHubHosted sums this month's GitHub-hosted Linux minutes from the billing
// usage report. Hosted runners are always online.
func (c *Collector) GitHubHosted() (policy.State, error) {
	org, err := c.ownerIsOrg()
	if err != nil {
		return policy.State{}, err
	}
	now := c.now()
	base := "/users/"
	if org {
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
	s := policy.State{Online: true}
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

// billedMinutes rounds a job's duration up to whole minutes and applies the
// Blacksmith size multiplier. Jobs not on a Blacksmith label bill nothing.
func billedMinutes(labels []string, started, completed time.Time) float64 {
	for _, l := range labels {
		if isBlacksmith(l) {
			d := completed.Sub(started)
			if d <= 0 {
				return 0
			}
			return math.Ceil(d.Minutes()) * blacksmithMultiplier(l)
		}
	}
	return 0
}

// Blacksmith estimates this month's billed Blacksmith minutes from the jobs of
// runs created this month in every repository the owner has, caching completed
// jobs in StatePath.
func (c *Collector) Blacksmith() (policy.State, error) {
	now := c.now()
	month := now.Format("2006-01")
	st, err := loadState(c.StatePath, month)
	if err != nil {
		return policy.State{}, err
	}
	repos, err := c.ownerRepos()
	if err != nil {
		return policy.State{}, err
	}
	var running float64
	for _, repo := range repos {
		runs, err := c.monthRuns(repo, now)
		if err != nil {
			return policy.State{}, err
		}
		for _, run := range runs {
			key := fmt.Sprintf("%d/%d", run.ID, run.RunAttempt)
			if st.Runs[key] {
				continue
			}
			jobs, err := c.jobs(repo, run.ID, "all")
			if err != nil {
				return policy.State{}, err
			}
			done := run.Status == "completed"
			for _, j := range jobs {
				id := strconv.FormatInt(j.ID, 10)
				if _, ok := st.Jobs[id]; ok {
					continue
				}
				switch {
				case j.Status == "completed" && j.StartedAt != nil && j.CompletedAt != nil:
					st.Jobs[id] = billedMinutes(j.Labels, *j.StartedAt, *j.CompletedAt)
				case j.Status == "completed":
					st.Jobs[id] = 0
				default:
					done = false
					if j.StartedAt != nil {
						running += billedMinutes(j.Labels, *j.StartedAt, now)
					}
				}
			}
			if done {
				st.Runs[key] = true
			}
		}
	}
	if err := st.save(c.StatePath); err != nil {
		return policy.State{}, err
	}
	s := policy.State{Online: true, UsedMinutes: running}
	for _, m := range st.Jobs {
		s.UsedMinutes += m
	}
	return s, nil
}

func (c *Collector) ownerRepos() ([]string, error) {
	org, err := c.ownerIsOrg()
	if err != nil {
		return nil, err
	}
	path := "/user/repos?affiliation=owner&per_page=100"
	if !org {
		// Only the authenticated user can list their own private repositories.
		var me struct {
			Login string `json:"login"`
		}
		if err := c.GH.Get("/user", &me); err != nil {
			return nil, err
		}
		if !strings.EqualFold(me.Login, c.Owner) {
			return nil, fmt.Errorf("user owner %s needs a token of that user, got %s", c.Owner, me.Login)
		}
	} else {
		path = "/orgs/" + url.PathEscape(c.Owner) + "/repos?type=all&per_page=100"
	}
	var repos []string
	err = c.GH.Paginate(path, func(body []byte) error {
		var page []struct {
			FullName string `json:"full_name"`
			Archived bool   `json:"archived"`
			Owner    struct {
				Login string `json:"login"`
			} `json:"owner"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		for _, r := range page {
			if !r.Archived && strings.EqualFold(r.Owner.Login, c.Owner) {
				repos = append(repos, r.FullName)
			}
		}
		return nil
	})
	return repos, err
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
