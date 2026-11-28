package discover

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/morhaf-ko/zerobill/internal/github"
)

type deadlineTransport struct{ workflowReads int }

func (d *deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Path, "/contents/") {
		d.workflowReads++
		return nil, context.DeadlineExceeded
	}
	body := `[{"full_name":"acme/a","private":true,"owner":{"login":"acme"}},{"full_name":"acme/b","private":true,"owner":{"login":"acme"}}]`
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestWorkflowTimeoutStopsScan(t *testing.T) {
	transport := &deadlineTransport{}
	gh := github.New("fixture-token")
	gh.HTTP.Transport = transport
	_, err := Discover(gh, "acme", true)
	if !errors.Is(err, context.DeadlineExceeded) || transport.workflowReads != 1 {
		t.Fatalf("retried after timeout: reads %d, err %v", transport.workflowReads, err)
	}
}

type resumeTransport struct {
	blocked     bool
	directories []string
}

func (d *resumeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	d.directories = append(d.directories, r.URL.Path)
	if d.blocked && strings.Contains(r.URL.Path, "/acme/b/") {
		return nil, context.DeadlineExceeded
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("[]"))}, nil
}

func TestWorkflowScanResumesAfterDeadline(t *testing.T) {
	transport := &resumeTransport{blocked: true}
	gh := github.New("fixture-token")
	gh.HTTP.Transport = transport
	repos := []github.Repo{{Name: "acme/a", Private: true}, {Name: "acme/b", Private: true}}
	var scanner Scanner
	if r, err := scanner.Scan(gh, repos); !errors.Is(err, context.DeadlineExceeded) || r.Scanned != 1 {
		t.Fatalf("interrupted scan: %+v, %v", r, err)
	}
	transport.blocked = false
	r, err := scanner.Scan(gh, repos)
	if err != nil || r.Scanned != 2 || len(r.Errors) != 0 || len(transport.directories) != 3 ||
		!strings.Contains(transport.directories[2], "/acme/b/") {
		t.Fatalf("resumed scan: %+v, %v, requests=%v", r, err, transport.directories)
	}
}
