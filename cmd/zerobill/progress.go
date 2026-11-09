package main

import (
	"fmt"
	"io"
	"strings"
	"time"
)

func statusProgress(w io.Writer) func(string) {
	var phase string
	var last time.Time
	requests := 0
	return func(path string) {
		label := "Checking configuration and GitHub credentials"
		if path != "" {
			requests++
			switch {
			case strings.Contains(path, "/contents/"):
				label = "Reading repository workflows"
			case strings.Contains(path, "/billing/"):
				label = "Reading GitHub usage"
			case strings.Contains(path, "/actions/runners"):
				label = "Reading runners"
			case strings.Contains(path, "/actions/runs/") && strings.HasSuffix(path, "/jobs"):
				label = "Reading Actions job history"
			case strings.Contains(path, "/actions/runs"):
				label = "Reading Actions workflow runs"
			case strings.HasSuffix(path, "/repos"):
				label = "Listing repositories"
			default:
				label = "Checking GitHub account access"
			}
		}
		if label != phase || time.Since(last) >= 5*time.Second {
			if requests == 0 {
				fmt.Fprintln(w, label+"... Press Ctrl+C to stop.")
			} else {
				fmt.Fprintf(w, "%s... (%d GitHub requests started)\n", label, requests)
			}
			phase, last = label, time.Now()
		}
	}
}
