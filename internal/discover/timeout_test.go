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
