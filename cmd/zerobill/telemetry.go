package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
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
	writeMu sync.Mutex
}

func (l *requestLog) flushOnSignal(w io.Writer) func() {
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-signals:
			l.flush(w)
			signal.Stop(signals)
			_ = syscall.Kill(os.Getpid(), sig.(syscall.Signal))
		case <-done:
		}
	}()
	return func() {
		l.flush(w)
		signal.Stop(signals)
		close(done)
	}
}

func (l *requestLog) record(o github.RequestObservation) {
	entry := requestLogEntry{Event: "github_request", Time: time.Now().UTC(), RequestObservation: o}
	l.mu.Lock()
	l.entries = append(l.entries, entry)
	l.mu.Unlock()
}

func (l *requestLog) flush(w io.Writer) {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
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
