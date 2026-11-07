package collect

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/morhaf-ko/zerobill/internal/github"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// fake serves canned JSON by path and counts requests per path.
type fake struct {
	t      *testing.T
	srv    *httptest.Server
	routes map[string]func(w http.ResponseWriter, r *http.Request)
	hits   map[string]int
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){}, hits: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("%s: missing bearer token", r.URL.Path)
		}
		f.hits[r.URL.Path]++
		h, ok := f.routes[r.URL.Path]
		if !ok {
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) json(path, body string) {
	f.routes[path] = func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }
}

func (f *fake) collector(owner string, repos ...string) *Collector {
	gh := github.New("test-token")
	gh.BaseURL = f.srv.URL
	return &Collector{GH: gh, Owner: owner, Repositories: repos, Now: func() time.Time { return now }}
}

func ts(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

func TestSelfHostedOrg(t *testing.T) {
	f := newFake(t)
	f.json("/users/acme", `{"type":"Organization"}`)
	f.routes["/orgs/acme/actions/runners"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `{"runners":[{"status":"online","labels":[{"name":"self-hosted"},{"name":"home-ci"}]}]}`)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/orgs/acme/actions/runners?per_page=100&page=2>; rel="next"`, f.srv.URL))
		fmt.Fprint(w, `{"runners":[{"status":"offline","labels":[{"name":"home-ci"}]},{"status":"online","labels":[{"name":"other"}]}]}`)
	}
	f.routes["/repos/acme/app/actions/runs"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") == "queued" {
			fmt.Fprint(w, `{"workflow_runs":[{"id":1,"run_attempt":1,"status":"queued"}]}`)
			return
		}
		fmt.Fprint(w, `{"workflow_runs":[{"id":2,"run_attempt":1,"status":"in_progress"}]}`)
	}
	f.json("/repos/acme/app/actions/runs/1/jobs", fmt.Sprintf(`{"jobs":[
		{"id":10,"status":"queued","labels":["home-ci"],"created_at":%q},
		{"id":11,"status":"queued","labels":["ubuntu-24.04"],"created_at":%q}]}`, ts(4*time.Minute), ts(time.Hour)))
	f.json("/repos/acme/app/actions/runs/2/jobs", fmt.Sprintf(`{"jobs":[
		{"id":20,"status":"in_progress","labels":["home-ci"],"created_at":%q},
		{"id":21,"status":"queued","labels":["home-ci"],"created_at":%q}]}`, ts(time.Hour), ts(12*time.Minute)))

	s, err := f.collector("acme", "acme/app").SelfHosted("home-ci")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Online || s.OldestQueued != 12*time.Minute {
		t.Fatalf("got %+v, want online with a 12m queue", s)
	}
	if f.hits["/orgs/acme/actions/runners"] != 2 {
		t.Fatalf("runner list not paginated: %d requests", f.hits["/orgs/acme/actions/runners"])
	}
}

func TestSelfHostedUserReadsRepositoryRunners(t *testing.T) {
	f := newFake(t)
	f.json("/users/octo", `{"type":"User"}`)
	f.json("/repos/octo/app/actions/runners", `{"runners":[{"status":"offline","labels":[{"name":"home-ci"}]}]}`)
	f.json("/repos/octo/app/actions/runs", `{"workflow_runs":[]}`)

	s, err := f.collector("octo", "octo/app").SelfHosted("home-ci")
	if err != nil {
		t.Fatal(err)
	}
	if s.Online || s.OldestQueued != 0 {
		t.Fatalf("got %+v, want offline with no queue", s)
	}
}

const usage = `{"usageItems":[
	{"product":"actions","sku":"Actions Linux","quantity":43,"unitType":"Minutes","repositoryName":"a"},
	{"product":"actions","sku":"Actions Linux","quantity":7,"unitType":"Minutes","repositoryName":"b"},
	{"product":"actions","sku":"Actions Windows","quantity":5,"unitType":"Minutes"},
	{"product":"actions","sku":"Actions storage","quantity":0.12,"unitType":"GigabyteHours"}]}`

func TestGitHubHosted(t *testing.T) {
	for _, tc := range []struct{ owner, kind, path string }{
		{"acme", "Organization", "/organizations/acme/settings/billing/usage"},
		{"octo", "User", "/users/octo/settings/billing/usage"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			f := newFake(t)
			f.json("/users/"+tc.owner, fmt.Sprintf(`{"type":%q}`, tc.kind))
			f.routes[tc.path] = func(w http.ResponseWriter, r *http.Request) {
				if q := r.URL.Query(); q.Get("year") != "2026" || q.Get("month") != "11" {
					t.Errorf("billing period %s, want 2026-11", r.URL.RawQuery)
				}
				fmt.Fprint(w, usage)
			}
			s, err := f.collector(tc.owner).GitHubHosted()
			if err != nil {
				t.Fatal(err)
			}
			if !s.Online || s.UsedMinutes != 50 {
				t.Fatalf("got %+v, want 50 Linux minutes", s)
			}
		})
	}
}

func TestBlacksmithMultiplier(t *testing.T) {
	for label, want := range map[string]float64{
		"blacksmith-2vcpu-ubuntu-2404":      1,
		"blacksmith-4vcpu-ubuntu-2204":      2,
		"blacksmith-32vcpu-ubuntu-2404-arm": 16,
		"blacksmith":                        1,
	} {
		if got := blacksmithMultiplier(label); got != want {
			t.Errorf("%s: got %v, want %v", label, got, want)
		}
	}
}

func TestBlacksmithCachesCompletedJobs(t *testing.T) {
	f := newFake(t)
	f.json("/users/acme", `{"type":"Organization"}`)
	f.json("/orgs/acme/repos", `[
		{"full_name":"acme/app","archived":false,"owner":{"login":"acme"}},
		{"full_name":"acme/old","archived":true,"owner":{"login":"acme"}}]`)
	f.routes["/repos/acme/app/actions/runs"] = func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("created"); got != ">=2026-11-01" {
			t.Errorf("created filter %q, want >=2026-11-01", got)
		}
		fmt.Fprint(w, `{"workflow_runs":[
			{"id":1,"run_attempt":1,"status":"completed"},
			{"id":2,"run_attempt":1,"status":"in_progress"}]}`)
	}
	f.routes["/repos/acme/app/actions/runs/1/jobs"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filter") != "all" {
			t.Errorf("jobs filter %q, want all attempts", r.URL.Query().Get("filter"))
		}
		fmt.Fprintf(w, `{"jobs":[
			{"id":10,"status":"completed","labels":["blacksmith-2vcpu-ubuntu-2404"],"started_at":%q,"completed_at":%q},
			{"id":11,"status":"completed","labels":["blacksmith-4vcpu-ubuntu-2404"],"started_at":%q,"completed_at":%q},
			{"id":12,"status":"completed","labels":["ubuntu-24.04"],"started_at":%q,"completed_at":%q},
			{"id":13,"status":"completed","labels":["blacksmith-2vcpu-ubuntu-2404"],"started_at":null,"completed_at":%q}]}`,
			ts(3*time.Hour), ts(3*time.Hour-61*time.Second),
			ts(2*time.Hour), ts(2*time.Hour-3*time.Minute),
			ts(time.Hour), ts(50*time.Minute),
			ts(time.Hour))
	}
	f.json("/repos/acme/app/actions/runs/2/jobs", fmt.Sprintf(`{"jobs":[
		{"id":20,"status":"completed","labels":["blacksmith-2vcpu-ubuntu-2404"],"started_at":%q,"completed_at":%q},
		{"id":21,"status":"in_progress","labels":["blacksmith-2vcpu-ubuntu-2404"],"started_at":%q}]}`,
		ts(10*time.Minute), ts(5*time.Minute), ts(90*time.Second)))

	c := f.collector("acme")
	c.StatePath = filepath.Join(t.TempDir(), "new", "state.json")
	// 61s on 2 vCPU bills 2, 3m on 4 vCPU bills 6, 5m bills 5, 90s running bills 2.
	for i := 0; i < 2; i++ {
		s, err := c.Blacksmith()
		if err != nil {
			t.Fatal(err)
		}
		if s.UsedMinutes != 15 {
			t.Fatalf("pass %d: got %v minutes, want 15", i, s.UsedMinutes)
		}
	}
	if n := f.hits["/repos/acme/app/actions/runs/1/jobs"]; n != 1 {
		t.Fatalf("completed run fetched %d times, want 1", n)
	}
	if n := f.hits["/repos/acme/app/actions/runs/2/jobs"]; n != 2 {
		t.Fatalf("in-progress run fetched %d times, want 2", n)
	}

	// A new month starts from an empty cache.
	st, err := loadState(c.StatePath, "2026-12")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Jobs) != 0 || len(st.Runs) != 0 {
		t.Fatalf("state not reset for a new month: %+v", st)
	}
}

func TestObserveRoutesProvidersAndReportsErrors(t *testing.T) {
	f := newFake(t)
	f.json("/users/acme", `{"type":"Organization"}`)
	f.json("/organizations/acme/settings/billing/usage", usage)
	f.routes["/orgs/acme/actions/runners"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
	}
	r := f.collector("acme").Observe([]policy.Provider{
		{Name: "home", Label: "home-ci", Kind: policy.SelfHosted},
		{Name: "github", Label: "ubuntu-24.04", Kind: policy.Metered},
		{Name: "typo", Label: "ubuntu24", Kind: policy.Metered},
	})
	if !strings.Contains(r.Errors["typo"], "no collector") {
		t.Fatalf("typo: %+v", r.Errors)
	}
	if _, ok := r.States["home"]; ok || !strings.Contains(r.Errors["home"], "403") {
		t.Fatalf("home: states %+v errors %+v", r.States, r.Errors)
	}
	if strings.Contains(r.Errors["home"], "test-token") {
		t.Fatal("error leaks the token")
	}
	if r.States["github"].UsedMinutes != 50 {
		t.Fatalf("github: %+v", r.States["github"])
	}
}

func TestBlacksmithSplitsMonthOverSearchCap(t *testing.T) {
	f := newFake(t)
	f.json("/users/acme", `{"type":"Organization"}`)
	f.json("/orgs/acme/repos", `[{"full_name":"acme/app","owner":{"login":"acme"}}]`)
	var days []string
	f.routes["/repos/acme/app/actions/runs"] = func(w http.ResponseWriter, r *http.Request) {
		created := r.URL.Query().Get("created")
		if created == ">=2026-11-01" {
			fmt.Fprint(w, `{"total_count":1500,"workflow_runs":[]}`)
			return
		}
		days = append(days, created)
		fmt.Fprint(w, `{"total_count":0,"workflow_runs":[]}`)
	}
	if _, err := f.collector("acme").Blacksmith(); err != nil {
		t.Fatal(err)
	}
	if len(days) != 7 || days[0] != "2026-11-01" || days[6] != "2026-11-07" {
		t.Fatalf("day queries %v, want 2026-11-01..07", days)
	}
}

func TestBlacksmithUserOwnerNeedsOwnToken(t *testing.T) {
	f := newFake(t)
	f.json("/users/octo", `{"type":"User"}`)
	f.json("/user", `{"login":"someone-else"}`)
	if _, err := f.collector("octo").Blacksmith(); err == nil || !strings.Contains(err.Error(), "needs a token") {
		t.Fatalf("expected a token owner error, got %v", err)
	}
}
