package main

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/morhaf-ko/zerobill/internal/github"
)

type requestLogEntry struct {
	Event string    `json:"event"`
	Time  time.Time `json:"time"`
	github.RequestObservation
}

// requestLog buffers observations until a check finishes so log I/O cannot stall GitHub calls.
type requestLog struct {
	mu      sync.Mutex
	entries []requestLogEntry
}

func (l *requestLog) record(o github.RequestObservation) {
	entry := requestLogEntry{Event: "github_request", Time: time.Now().UTC(), RequestObservation: o}
	l.mu.Lock()
	l.entries = append(l.entries, entry)
	l.mu.Unlock()
}

func (l *requestLog) flush(w io.Writer) {
	l.mu.Lock()
	entries := l.entries
	l.entries = nil
	l.mu.Unlock()
	if len(entries) == 0 {
		return
	}
	var batch bytes.Buffer
	encoder := json.NewEncoder(&batch)
	for _, entry := range entries {
		_ = encoder.Encode(entry)
	}
	_, _ = batch.WriteTo(w)
}
