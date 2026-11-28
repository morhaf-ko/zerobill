package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

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

func TestRequestLogFlushesOnTermination(t *testing.T) {
	if os.Getenv("ZEROBILL_TEST_TERMINATION") == "1" {
		var log requestLog
		defer log.flushOnSignal(os.Stderr)()
		log.record(github.RequestObservation{Endpoint: "login", Method: "GET", Status: 200})
		fmt.Println("ready")
		time.Sleep(time.Minute)
		return
	}
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRequestLogFlushesOnTermination$")
			cmd.Env = append(os.Environ(), "ZEROBILL_TEST_TERMINATION=1")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			var ready string
			if _, err := fmt.Fscan(stdout, &ready); err != nil || ready != "ready" {
				_ = cmd.Wait()
				t.Fatalf("child readiness = %q, error = %v", ready, err)
			}
			if err := cmd.Process.Signal(sig); err != nil {
				_ = cmd.Wait()
				t.Fatal(err)
			}
			if err := cmd.Wait(); err == nil || cmd.ProcessState.Sys().(syscall.WaitStatus).Signal() != sig {
				t.Fatalf("child should terminate with %s, got %v", sig, err)
			}
			if !strings.Contains(stderr.String(), `"endpoint":"login","method":"GET","status":200`) {
				t.Fatalf("pending request missing from stderr: %q", stderr.String())
			}
		})
	}
}
