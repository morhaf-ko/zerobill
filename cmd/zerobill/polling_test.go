package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/config"
	"github.com/morhaf-ko/zerobill/internal/github"
)

type pollingTransport func(*http.Request) (*http.Response, error)

func (f pollingTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPollingRequestBudget(t *testing.T) {
	for _, mode := range []string{"quiet", "busy", "rollover"} {
		t.Run(mode, func(t *testing.T) {
			start := time.Date(2026, 11, 30, 22, 0, 0, 0, time.UTC)
			if mode == "rollover" {
				start = start.Add(90 * time.Minute)
			}
			at := start.Add(-time.Minute)
			tick := -1
			attempt := 0
			counts := make(map[string]map[int]int)
			var inventory []map[string]any
			for i := range 15 {
				inventory = append(inventory, map[string]any{"full_name": fmt.Sprintf("org/repo-%d", i), "private": true, "owner": map[string]string{"login": "org"}})
			}
			repos, err := json.Marshal(inventory)
			if err != nil {
				t.Fatal(err)
			}
			gh := github.New("fixture")
			gh.EnableConditionalReads()
			gh.ObserveRequest = func(o github.RequestObservation) {
				if tick < 0 {
					return
				}
				key := o.Method + " " + o.Endpoint
				if counts[key] == nil {
					counts[key] = make(map[int]int)
				}
				counts[key][o.Status]++
			}
			gh.HTTP.Transport = pollingTransport(func(r *http.Request) (*http.Response, error) {
				path := r.URL.Path
				res := &http.Response{StatusCode: 200, Header: make(http.Header)}
				var body string
				switch {
				case path == "/orgs/org/repos":
					body = string(repos)
				case path == "/orgs/org/actions/runners":
					body = `{"runners":[{"status":"online","labels":[{"name":"home"}]}]}`
				case path == "/organizations/org/settings/billing/usage":
					body = `{"usageItems":[]}`
				case strings.HasSuffix(path, "/contents/.github/workflows"):
					body = `[{"name":"test.yml","path":".github/workflows/test.yml","type":"file","sha":"one"},{"name":"release.yml","path":".github/workflows/release.yml","type":"file","sha":"two"}]`
				case strings.Contains(path, "/contents/"):
					sha := "one"
					if strings.HasSuffix(path, "release.yml") {
						sha = "two"
					}
					body = fmt.Sprintf(`{"encoding":"base64","sha":%q,"content":%q}`, sha, base64.StdEncoding.EncodeToString([]byte("jobs: {build: {runs-on: '\x24{{ vars.CI_RUNNER }}'}}")))
				case strings.HasSuffix(path, "/actions/runs"):
					body = `{"workflow_runs":[]}`
					if mode == "busy" && strings.Contains(path, "/repo-0/") && attempt > 0 {
						if r.URL.Query().Has("created") {
							body = fmt.Sprintf(`{"workflow_runs":[{"id":1,"run_attempt":%d,"status":"completed"}]}`, attempt)
						}
						if tick%10 < 2 && r.URL.Query().Get("status") == "in_progress" {
							body = fmt.Sprintf(`{"workflow_runs":[{"id":1,"run_attempt":%d,"status":"in_progress"}]}`, attempt)
						}
					}
				case strings.HasSuffix(path, "/runs/1/jobs"):
					body = fmt.Sprintf(`{"jobs":[{"id":%d,"run_attempt":%d,"status":"completed","conclusion":"success"}]}`, attempt, attempt)
				case strings.HasSuffix(path, "/variables/CI_RUNNER"):
					body = `{"value":"home"}`
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					res.StatusCode = 404
				}
				if !strings.Contains(path, "billing/usage") {
					etag := fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(body)))
					res.Header.Set("ETag", etag)
					if r.Header.Get("If-None-Match") == etag {
						res.StatusCode, body = 304, ""
					}
				}
				res.Body = io.NopCloser(strings.NewReader(body))
				return res, nil
			})
			cfg := &config.Config{Owner: "org", SelfHostedLabel: "home", Blacksmith: true, CheckInterval: time.Minute}
			// Keep both hosted collectors enabled so their requests appear in the budget.
			a := &app{cfg: cfg, gh: gh, org: true, stateDir: t.TempDir()}
			a.providers, a.routes = build(cfg, 2000)
			a.col = &collect.Collector{GH: gh, Owner: cfg.Owner, Org: true, StatePath: filepath.Join(a.stateDir, "cache.json"), Now: func() time.Time { return at }}
			c, err := newController(a, true)
			if err != nil {
				t.Fatal(err)
			}
			for ; tick < 60; tick++ {
				at = start.Add(time.Duration(tick) * time.Minute)
				if mode == "busy" && tick >= 0 {
					attempt = 1 + tick/10
				}
				var output strings.Builder
				if err := c.check(&output, at); err != nil {
					t.Fatal(err)
				}
				var result line
				if err := json.Unmarshal([]byte(output.String()), &result); err != nil || (tick >= 0 && len(result.Errors) != 0) {
					t.Fatalf("tick %d: %v %s", tick, err, output.String())
				}
			}
			total, charged := 0, 0
			for _, statuses := range counts {
				for code, n := range statuses {
					total += n
					if code != 304 {
						charged += n
					}
				}
			}
			t.Logf("60 ticks: attempts=%d charged=%d by endpoint/status=%v", total, charged, counts)
			if mode == "quiet" && total != 3075 {
				t.Errorf("quiet attempts = %d, want 3075 after sharing inventory and reconciling stable variables and workflow SHAs", total)
			}
			if charged >= 2500 || counts["GET runs_queued"][304] == 0 {
				t.Fatalf("fixture should observe fewer than 2,500 potentially charged requests with revalidation, got %d", charged)
			}
			if counts["GET runs_queued"][200]+counts["GET runs_queued"][304] != 900 ||
				counts["GET runs_in_progress"][200]+counts["GET runs_in_progress"][304] != 900 {
				t.Fatal("every repository needs both active status queries in every tick")
			}
		})
	}
}
