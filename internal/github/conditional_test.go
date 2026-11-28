package github

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestConditionalPaginationRevalidatesCurrentChain(t *testing.T) {
	gh := New("fixture")
	gh.EnableConditionalReads()
	const first = "/orgs/org/actions/runners?per_page=100"
	const second = "/orgs/org/actions/runners?page=2"
	calls := 0
	gh.HTTP.Transport = telemetryTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		wantTags := []string{"", "", `"first"`, `"second"`, `"first"`, `"short"`, ""}
		if got := r.Header.Get("If-None-Match"); calls > len(wantTags) || got != wantTags[calls-1] {
			t.Fatalf("request %d If-None-Match=%q", calls, got)
		}
		response := &http.Response{StatusCode: 200, Header: make(http.Header)}
		body := `{"runners":[1]}`
		switch calls {
		case 1, 6:
			response.Header.Set("ETag", `"first"`)
			response.Header.Set("Link", fmt.Sprintf("<%s%s>; rel=\"next\"", gh.BaseURL, second))
		case 2, 7:
			response.Header.Set("ETag", `"second"`)
			body = `{"runners":[2]}`
		case 3, 4:
			response.StatusCode, body = 304, ""
		case 5:
			response.Header.Set("ETag", `"short"`)
		}
		response.Body = io.NopCloser(strings.NewReader(body))
		return response, nil
	})
	for _, want := range [][]string{
		{`{"runners":[1]}`, `{"runners":[2]}`},
		{`{"runners":[1]}`, `{"runners":[2]}`},
		{`{"runners":[1]}`},
		{`{"runners":[1]}`, `{"runners":[2]}`},
	} {
		var got []string
		err := gh.ConditionalPaginate(first, func(raw []byte) error {
			got = append(got, string(raw))
			return nil
		})
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("pages %v, want %v, err=%v", got, want, err)
		}
	}
}

func TestConditionalReadRetriesUnusable304AndInvalidatesAfterWrite(t *testing.T) {
	gh := New("fixture")
	gh.EnableConditionalReads()
	const path = "/repos/org/repo/actions/variables/CI_RUNNER"
	calls := 0
	gh.HTTP.Transport = telemetryTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if value := r.Header.Get("If-None-Match"); value != "" {
			t.Fatalf("call %d unexpectedly sends validator %s", calls, value)
		}
		response := &http.Response{StatusCode: 200, Header: make(http.Header)}
		body := `{"value":"home"}`
		switch calls {
		case 1:
			response.StatusCode, body = 304, ""
		case 3:
			response.StatusCode, body = 204, ""
		default:
			response.Header.Set("ETag", `"tag"`)
		}
		response.Body = io.NopCloser(strings.NewReader(body))
		return response, nil
	})
	var value map[string]string
	if err := gh.ConditionalGet(path, &value); err != nil || value["value"] != "home" || calls != 2 {
		t.Fatalf("unusable 304: %v (%v, %d attempts)", err, value, calls)
	}
	if err := gh.Send(http.MethodPatch, path, nil); err != nil {
		t.Fatal(err)
	}
	if err := gh.ConditionalGet(path, &value); err != nil || calls != 4 {
		t.Fatalf("read after write: %v (%d attempts)", err, calls)
	}
}

func TestMissingWorkflowDirectoryKeepsOtherRepoValidators(t *testing.T) {
	gh := New("fixture")
	gh.EnableConditionalReads()
	const path = "/repos/org/repo/actions/runs?status=queued"
	gh.HTTP.Transport = telemetryTestTransport(func(r *http.Request) (*http.Response, error) {
		resp := &http.Response{StatusCode: 200, Header: make(http.Header)}
		body := `{"workflow_runs":[]}`
		switch {
		case strings.Contains(r.URL.Path, "/contents/"):
			resp.StatusCode, body = 404, "{}"
		case r.Header.Get("If-None-Match") == `"runs"`:
			resp.StatusCode, body = 304, ""
		default:
			resp.Header.Set("ETag", `"runs"`)
		}
		resp.Body = io.NopCloser(strings.NewReader(body))
		return resp, nil
	})
	var status int
	gh.ObserveRequest = func(o RequestObservation) { status = o.Status }
	var out map[string]any
	if err := gh.ConditionalGet(path, &out); err != nil {
		t.Fatal(err)
	}
	if err := gh.ConditionalGet("/repos/org/repo/contents/.github/workflows", &out); !IsNotFound(err) {
		t.Fatalf("directory error = %v", err)
	}
	if err := gh.ConditionalGet(path, &out); err != nil || status != 304 {
		t.Fatalf("run-list revalidation = %d, %v", status, err)
	}
}
