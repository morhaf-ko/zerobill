package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestEndpointClass(t *testing.T) {
	for path, want := range map[string]string{
		"/user":                                                   "login",
		"/users/owner":                                            "owner_type",
		"/orgs/owner":                                             "org_plan",
		"/user/repos?affiliation=owner":                           "repo_inventory",
		"/orgs/owner/repos?type=all":                              "repo_inventory",
		"/orgs/owner/actions/runners":                             "org_runners",
		"/repos/owner/repo/actions/runners":                       "repo_runners",
		"/repos/owner/repo/actions/runs?status=queued":            "runs_queued",
		"/repos/owner/repo/actions/runs?status=in_progress":       "runs_in_progress",
		"/repos/owner/repo/actions/runs?created=%3E%3D2026-11-01": "runs_created",
		"/repos/owner/repo/actions/runs/123/jobs?filter=latest":   "jobs_latest",
		"/repos/owner/repo/actions/runs/123/jobs?filter=all":      "jobs_all",
		"/organizations/owner/settings/billing/usage":             "billing_usage",
		"/users/owner/settings/billing/usage":                     "billing_usage",
		"/repos/owner/repo/contents/.github/workflows":            "workflow_directory",
		"/repos/owner/repo/contents/.github/workflows/ci.yml":     "workflow_file",
		"/repos/owner/repo/actions/variables":                     "variables",
		"/repos/owner/repo/actions/variables/CI_RUNNER":           "variable",
		"/repos/owner/repo/actions/runs/123":                      "run",
		"/repos/owner/repo/actions/runs/123/attempts/2/jobs":      "attempt_jobs",
		"/repos/owner/repo/actions/runs/123/cancel":               "cancel",
		"/repos/owner/repo/actions/runs/123/force-cancel":         "force_cancel",
		"/repos/owner/repo/actions/runs/123/rerun-failed-jobs":    "rerun_failed_jobs",
		"/repos/owner/repo/actions/runs/123/private":              "other",
	} {
		t.Run(path, func(t *testing.T) {
			u, err := url.Parse(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := endpointClass(u); got != want {
				t.Fatalf("class = %q, want %q", got, want)
			}
		})
	}
}

type telemetryTestTransport func(*http.Request) (*http.Response, error)

func (f telemetryTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestRequestObservation(t *testing.T) {
	const secret = "fixture-secret"
	const private = "private-repo"
	c := New(secret)
	var observations []RequestObservation
	c.ObserveRequest = func(o RequestObservation) {
		observations = append(observations, o)
	}
	var calls int
	c.HTTP.Transport = telemetryTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("missing request authorization")
		}
		h := make(http.Header)
		status, body := 200, `[]`
		switch calls {
		case 1:
			status = 301
			h.Set("Location", "/repositories/123/actions/runs?status=queued")
		case 2:
			h.Set("Link", `<https://api.github.com/repositories/123/actions/runs?page=2>; rel="next"`)
			h.Set("X-RateLimit-Limit", "5000")
			h.Set("X-RateLimit-Remaining", "0")
			h.Set("X-RateLimit-Used", "5000")
			h.Set("X-RateLimit-Reset", "1800000000")
			h.Set("X-RateLimit-Resource", "core")
			h.Set("ETag", secret)
		case 3:
			status = 304
		case 4:
			status = 403
			body = fmt.Sprintf(`{"message":%q}`, private)
			h.Set("X-RateLimit-Limit", "-1")
			h.Set("X-RateLimit-Remaining", secret)
			h.Set("X-RateLimit-Used", "9999999999999999999999")
			h.Set("X-RateLimit-Resource", private)
		case 5:
			status = 429
		case 6:
			return nil, errors.New(secret)
		}
		return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: h, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	base := "/repos/owner/" + private + "/actions"
	err := c.Paginate(base+"/runs?status=queued", func([]byte) error { return nil })
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != 304 {
		t.Fatalf("pagination error = %v, want 304", err)
	}
	if err := c.Send(http.MethodPost, base+"/variables", map[string]string{"value": secret}); !IsAccessDenied(err) {
		t.Fatalf("write error = %v, want 403", err)
	}
	if err := c.Send(http.MethodPatch, base+"/variables/CI_RUNNER", nil); !IsRateLimited(err) {
		t.Fatalf("write error = %v, want 429", err)
	}
	if err := c.Get(base+"/runs?status=in_progress", nil); err == nil {
		t.Fatal("expected transport error")
	}
	if len(observations) != 6 || calls != 6 {
		t.Fatalf("observations = %d, calls = %d, want 6 of each", len(observations), calls)
	}
	want := []struct {
		endpoint, method string
		status           int
	}{
		{"runs_queued", "GET", 301},
		{"runs_queued", "GET", 200},
		{"runs_queued", "GET", 304},
		{"variables", "POST", 403},
		{"variable", "PATCH", 429},
		{"runs_in_progress", "GET", 0},
	}
	for i, o := range observations {
		if o.Endpoint != want[i].endpoint || o.Method != want[i].method || o.Status != want[i].status {
			t.Errorf("observation %d = %+v, want %+v", i, o, want[i])
		}
	}
	quota, err := json.Marshal(observations[1].RateLimit)
	if err != nil || string(quota) != `{"limit":5000,"remaining":0,"used":5000,"reset":1800000000,"resource":"core"}` {
		t.Fatalf("quota = %s, error = %v", quota, err)
	}
	if !observations[1].ETag || observations[3].RateLimit != nil {
		t.Fatalf("validator or malformed quota recorded incorrectly: %+v", observations)
	}
	encoded, err := json.Marshal(observations)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{secret, private, "owner/", "repositories/123", "CI_RUNNER"} {
		if strings.Contains(string(encoded), value) {
			t.Errorf("observations contain %q: %s", value, encoded)
		}
	}
}
