package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/config"
	"github.com/morhaf-ko/zerobill/internal/discover"
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

func TestPollingFailsOverAndVerifiesReplacement(t *testing.T) {
	const repo = "org/repo"
	const prefix = "/repos/" + repo
	const runPath = prefix + "/actions/runs/1"
	const sha = "0123456789012345678901234567890123456789"
	start := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	at := start
	tick, stage := 0, 0
	var posts []string
	warm := `{"runners":[]}`
	attemptOne := func(status, conclusion string) string {
		return fmt.Sprintf(`{"total_count":1,"jobs":[{"id":10,"name":"build","status":%q,"conclusion":%q,"run_attempt":1,"created_at":%q,"labels":["home"]}]}`, status, conclusion, start.Format(time.RFC3339))
	}
	vars := &variables{values: map[string]string{repo + "/CI_RUNNER": "home"}}
	gh := github.New("fixture")
	gh.EnableConditionalReads()
	gh.HTTP.Transport = pollingTransport(func(r *http.Request) (*http.Response, error) {
		path := r.URL.Path
		resp := &http.Response{StatusCode: 200, Header: make(http.Header)}
		var body string
		switch path {
		case "/orgs/org/actions/runners":
			body = warm
		case "/organizations/org/settings/billing/usage":
			body = `{"usageItems":[]}`
		case prefix + "/actions/runs":
			body = `{"workflow_runs":[]}`
			if stage == 0 && r.URL.Query().Get("status") == "in_progress" {
				body = fmt.Sprintf(`{"workflow_runs":[{"id":1,"run_attempt":1,"status":"in_progress","created_at":%q}]}`, start.Format(time.RFC3339))
			}
		case runPath + "/jobs":
			body = `{"jobs":[]}`
			if tick > 0 {
				body = attemptOne("queued", "")
			}
		case runPath + "/attempts/1/jobs":
			body = attemptOne("queued", "")
			if stage == 1 {
				body = attemptOne("completed", "cancelled")
			}
		case runPath + "/attempts/2/jobs":
			body = `{"total_count":1,"jobs":[{"id":20,"name":"build","status":"in_progress","run_attempt":2,"labels":["ubuntu-latest"],"runner_name":"fixture-hosted"}]}`
		case runPath:
			status, conclusion, attempt := "in_progress", "", 1
			switch stage {
			case 1:
				status, conclusion = "completed", "cancelled"
			case 2:
				attempt = 2
			}
			body = fmt.Sprintf(`{"status":%q,"conclusion":%q,"run_attempt":%d,"path":".github/workflows/test.yml","event":"push","head_sha":%q}`, status, conclusion, attempt, sha)
		case prefix + "/contents/.github/workflows/test.yml":
			if r.URL.Query().Get("ref") != sha || r.Header.Get("If-None-Match") != "" {
				t.Error("recovery needs the exact workflow revision and a fresh read")
			}
			body = fmt.Sprintf(`{"encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte("jobs: {build: {runs-on: '\x24{{ vars.CI_RUNNER }}'}}")))
		case prefix + "/actions/variables/CI_RUNNER":
			recorder := httptest.NewRecorder()
			vars.ServeHTTP(recorder, r)
			resp.StatusCode, body = recorder.Code, recorder.Body.String()
		case runPath + "/cancel", runPath + "/rerun-failed-jobs":
			if r.Method != http.MethodPost || vars.values[repo+"/CI_RUNNER"] != githubLabel {
				t.Fatalf("recovery acted before switching the variable: %s %s %v", r.Method, path, vars.values)
			}
			posts = append(posts, strings.TrimPrefix(path, runPath+"/"))
			stage++
			resp.StatusCode = http.StatusAccepted
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			resp.StatusCode = 404
		}
		if resp.StatusCode == 200 {
			tag := fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(body)))
			resp.Header.Set("ETag", tag)
			if r.Header.Get("If-None-Match") == tag {
				resp.StatusCode, body = 304, ""
			}
		}
		resp.Body = io.NopCloser(strings.NewReader(body))
		return resp, nil
	})
	cfg := &config.Config{Owner: "org", SelfHostedLabel: "home", QueueTimeout: time.Minute, CheckInterval: time.Minute}
	a := &app{cfg: cfg, gh: gh, stateDir: t.TempDir(), org: true}
	a.providers, a.routes = build(cfg, 2000)
	a.col = &collect.Collector{GH: gh, Owner: cfg.Owner, Org: true, Now: func() time.Time { return at }}
	c, err := newController(a, true)
	if err != nil {
		t.Fatal(err)
	}
	c.repos = []discover.Repo{{Name: repo, Vars: []string{"CI_RUNNER"}}}
	c.foundAt = start
	for tick = 0; tick < 6; tick++ {
		at = start.Add(time.Duration(tick) * time.Minute)
		if tick == 5 {
			warm = `{"runners":[{"status":"online","labels":[{"name":"home"}]}]}`
		}
		var output strings.Builder
		if err := c.check(&output, at); err != nil {
			t.Fatal(err)
		}
		var result line
		if err := json.Unmarshal([]byte(output.String()), &result); err != nil || len(result.Errors) != 0 {
			t.Fatalf("tick %d: %v %s", tick, err, output.String())
		}
		wantLabel := "home"
		if tick >= 2 && tick <= 4 {
			wantLabel = githubLabel
		}
		if vars.values[repo+"/CI_RUNNER"] != wantLabel || len(posts) != min(2, max(0, tick-1)) {
			t.Fatalf("tick %d: variables=%v recovery=%v", tick, vars.values, posts)
		}
		if tick == 4 && (len(result.Rerun) != 1 || len(c.state.Cancelled) != 0) {
			t.Fatalf("replacement was never verified: %+v", result)
		}
	}
	if strings.Join(posts, ",") != "cancel,rerun-failed-jobs" {
		t.Fatalf("recovery actions=%v", posts)
	}
}
