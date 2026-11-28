package github

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// RequestObservation describes one transport attempt. Status zero means no HTTP response.
type RequestObservation struct {
	Endpoint   string     `json:"endpoint"`
	Method     string     `json:"method"`
	Status     int        `json:"status"`
	DurationMS int64      `json:"duration_ms"`
	RateLimit  *RateLimit `json:"rate_limit,omitempty"`
	ETag       bool       `json:"etag,omitempty"`
}

// RateLimit reflects the shared credential bucket at the time GitHub responded.
type RateLimit struct {
	Limit     *uint64 `json:"limit,omitempty"`
	Remaining *uint64 `json:"remaining,omitempty"`
	Used      *uint64 `json:"used,omitempty"`
	Reset     *uint64 `json:"reset,omitempty"`
	Resource  string  `json:"resource,omitempty"`
}

type observedTransport struct {
	base     http.RoundTripper
	endpoint string
	observe  func(RequestObservation)
}

func (t observedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	started := time.Now()
	resp, err := t.base.RoundTrip(req)
	o := RequestObservation{Endpoint: t.endpoint, Method: "OTHER", DurationMS: time.Since(started).Milliseconds()}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete, http.MethodOptions:
		o.Method = req.Method
	}
	if resp != nil {
		o.Status = resp.StatusCode
		o.RateLimit = rateLimit(resp.Header)
		o.ETag = resp.Header.Get("ETag") != ""
	}
	t.observe(o)
	return resp, err
}

func rateLimit(h http.Header) *RateLimit {
	number := func(name string) *uint64 {
		n, err := strconv.ParseUint(h.Get("X-RateLimit-"+name), 10, 64)
		if err != nil {
			return nil
		}
		return &n
	}
	q := RateLimit{
		Limit: number("Limit"), Remaining: number("Remaining"),
		Used: number("Used"), Reset: number("Reset"),
	}
	// All current client endpoints are in GitHub's core REST bucket.
	if h.Get("X-RateLimit-Resource") == "core" {
		q.Resource = "core"
	}
	if q == (RateLimit{}) {
		return nil
	}
	return &q
}

func endpointClass(u *url.URL) string {
	parts := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	switch parts[0] {
	case "user":
		switch strings.Join(parts[1:], "/") {
		case "":
			return "login"
		case "repos":
			return "repo_inventory"
		}
	case "orgs":
		if len(parts) >= 2 {
			switch strings.Join(parts[2:], "/") {
			case "":
				return "org_plan"
			case "repos":
				return "repo_inventory"
			case "actions/runners":
				return "org_runners"
			}
		}
	case "users", "organizations":
		if len(parts) == 2 && parts[0] == "users" {
			return "owner_type"
		}
		if len(parts) == 5 && strings.Join(parts[2:], "/") == "settings/billing/usage" {
			return "billing_usage"
		}
	case "repos":
		if len(parts) > 3 {
			return repoEndpoint(parts[3:], u.Query())
		}
	}
	return "other"
}

func repoEndpoint(parts []string, query url.Values) string {
	path := strings.Join(parts, "/")
	switch path {
	case "actions/runners":
		return "repo_runners"
	case "actions/variables":
		return "variables"
	case "contents/.github/workflows":
		return "workflow_directory"
	case "actions/runs":
		switch query.Get("status") {
		case "queued":
			return "runs_queued"
		case "in_progress":
			return "runs_in_progress"
		}
		if query.Has("created") {
			return "runs_created"
		}
	}
	switch {
	case strings.HasPrefix(path, "contents/"):
		return "workflow_file"
	case len(parts) == 3 && parts[0] == "actions" && parts[1] == "variables":
		return "variable"
	case strings.HasPrefix(path, "actions/runs/"):
		switch {
		case len(parts) == 3:
			return "run"
		case len(parts) == 4:
			switch parts[3] {
			case "jobs":
				switch query.Get("filter") {
				case "latest":
					return "jobs_latest"
				case "all":
					return "jobs_all"
				}
			case "cancel":
				return "cancel"
			case "force-cancel":
				return "force_cancel"
			case "rerun-failed-jobs":
				return "rerun_failed_jobs"
			}
		case len(parts) == 6 && parts[3] == "attempts" && parts[5] == "jobs":
			return "attempt_jobs"
		}
	}
	return "other"
}
