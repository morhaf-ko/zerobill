package main

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/morhaf-ko/zerobill/internal/github"
)

func TestRequestLogFlushesEachAttemptOnce(t *testing.T) {
	var log requestLog
	var output bytes.Buffer
	for _, status := range []int{200, 304, 403, 429, 0} {
		log.record(github.RequestObservation{Endpoint: "runs_queued", Method: "GET", Status: status})
	}
	log.flush(&output)
	log.flush(&output)

	d := json.NewDecoder(&output)
	for _, want := range []int{200, 304, 403, 429, 0} {
		var got requestLogEntry
		if err := d.Decode(&got); err != nil {
			t.Fatalf("status %d: %v", want, err)
		}
		if got.Status != want || got.Event != "github_request" || got.Time.IsZero() || got.Endpoint != "runs_queued" || got.Method != "GET" {
			t.Fatalf("status %d: unexpected entry %+v", want, got)
		}
	}
	if err := d.Decode(new(requestLogEntry)); err != io.EOF {
		t.Fatalf("expected no duplicate records, got %v", err)
	}
}
