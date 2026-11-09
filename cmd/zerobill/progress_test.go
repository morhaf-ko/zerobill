package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatusProgress(t *testing.T) {
	var out bytes.Buffer
	progress := statusProgress(&out)
	progress("")
	if !strings.Contains(out.String(), "Checking configuration and GitHub credentials") || !strings.Contains(out.String(), "Ctrl+C") {
		t.Fatal("no immediate progress")
	}
	progress("/user")
	for i := 0; i < 10; i++ {
		progress("/repos/private-owner/private-repo/contents/.github/workflows/ci.yml")
	}
	progress("/organizations/private-owner/settings/billing/usage")
	for _, want := range []string{"Reading repository workflows", "Reading GitHub usage", "12 GitHub requests started"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %s: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "private-owner") || strings.Contains(out.String(), "private-repo") {
		t.Fatal("progress exposed repository identifiers")
	}
}

func TestCredentialLookupDeadline(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := githubToken(ctx); err == nil || !strings.Contains(err.Error(), "credential lookup timed out") || !strings.Contains(err.Error(), "gh auth status") {
		t.Fatalf("no credential timeout guidance: %v", err)
	}
}
