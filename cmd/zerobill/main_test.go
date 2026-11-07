package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/morhaf-ko/zerobill/internal/apply"
	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/github"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

var now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func TestDryRunLogsOneJSONLine(t *testing.T) {
	cfg, providers, err := loadConfig("../../examples/zerobill.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r := collect.Result{
		States: map[string]policy.State{"blacksmith": {Online: true, UsedMinutes: 1240}, "github": {Online: true, UsedMinutes: 310}},
		Errors: map[string]string{"home": "GET /orgs/your-org/actions/runners: 403 Forbidden"},
	}
	var buf bytes.Buffer
	if err := (&controller{cfg: cfg, providers: providers}).step(&buf, now, r); err != nil {
		t.Fatal(err)
	}
	line := decode(t, &buf)
	if line.Provider != "blacksmith" || line.Label != "blacksmith-2vcpu-ubuntu-2404" || line.Errors["home"] == "" || line.Written != nil {
		t.Fatalf("unexpected line %+v", line)
	}
	if *line.Observed["github"].UsedMinutes != 310 || line.Observed["home"].Online != nil {
		t.Fatalf("unexpected observations %+v", line.Observed)
	}
}

func TestSwitchModeAppliesHoldsAndSkips(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "zerobill.yaml")
	raw, err := os.ReadFile("../../examples/zerobill.yaml")
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte("mode: dry-run"), []byte("mode: switch"), 1)
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, providers, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch r.Method {
		case http.MethodGet:
			v, ok := values[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprintf(w, `{"value":%q}`, v)
		default:
			writes++
			var in struct{ Name, Value string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			values[in.Name] = in.Value
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	gh := github.New("t")
	gh.BaseURL = srv.URL
	appliedPath := filepath.Join(dir, "applied.json")
	ctl := &controller{cfg: cfg, providers: providers, writer: &apply.Writer{GH: gh}, appliedPath: appliedPath}
	homeDown := map[string]policy.State{"home": {}, "blacksmith": {UsedMinutes: 100}, "github": {}}
	homeUp := map[string]policy.State{"home": {Online: true}, "blacksmith": {UsedMinutes: 100}, "github": {}}

	steps := []struct {
		name    string
		at      time.Duration
		result  collect.Result
		label   string
		held    bool
		written int
	}{
		{"home offline spills to blacksmith", 0, collect.Result{States: homeDown}, "blacksmith-2vcpu-ubuntu-2404", false, 3},
		{"unchanged decision writes nothing", time.Minute, collect.Result{States: homeDown}, "blacksmith-2vcpu-ubuntu-2404", false, 0},
		{"home back inside hold stays", 5 * time.Minute, collect.Result{States: homeUp}, "blacksmith-2vcpu-ubuntu-2404", true, 0},
		{"collector failure writes nothing", 16 * time.Minute, collect.Result{States: homeUp, Errors: map[string]string{"github": "403"}}, "home-ci", false, -1},
		{"home back after hold switches", 17 * time.Minute, collect.Result{States: homeUp}, "home-ci", false, 3},
	}
	for _, s := range steps {
		writes = 0
		var buf bytes.Buffer
		if err := ctl.step(&buf, now.Add(s.at), s.result); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		line := decode(t, &buf)
		if line.Label != s.label || line.Held != s.held {
			t.Fatalf("%s: got %s held=%v (%s)", s.name, line.Label, line.Held, line.Reason)
		}
		if s.written < 0 {
			if writes != 0 || line.Written != nil || line.Errors["write"] == "" {
				t.Fatalf("%s: wrote %d, line %+v", s.name, writes, line)
			}
			continue
		}
		if len(line.Written) != s.written || writes != s.written {
			t.Fatalf("%s: written %v, %d requests", s.name, line.Written, writes)
		}
	}
	a, err := apply.LoadApplied(appliedPath)
	if err != nil || a.Label != "home-ci" || !a.Since.Equal(now.Add(17*time.Minute)) {
		t.Fatalf("applied %+v %v", a, err)
	}
}

func TestSwitchModePartialWriteStartsHold(t *testing.T) {
	cfg, providers, err := loadConfig("../../examples/zerobill.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Mode = "switch"
	cfg.Repositories = []string{"acme/ok", "acme/denied"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/denied/"):
			w.WriteHeader(http.StatusForbidden)
		case r.Method == http.MethodGet:
			fmt.Fprint(w, `{"value":"home-ci"}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	gh := github.New("t")
	gh.BaseURL = srv.URL
	ctl := &controller{
		cfg: cfg, providers: providers, writer: &apply.Writer{GH: gh},
		appliedPath: filepath.Join(t.TempDir(), "applied.json"),
		applied:     policy.Applied{Provider: "home", Label: "home-ci", Since: now.Add(-time.Hour)},
	}
	var buf bytes.Buffer
	states := map[string]policy.State{"home": {}, "blacksmith": {UsedMinutes: 100}, "github": {}}
	if err := ctl.step(&buf, now, collect.Result{States: states}); err != nil {
		t.Fatal(err)
	}
	line := decode(t, &buf)
	if len(line.Written) != 3 || !strings.Contains(line.Errors["write"], "403") {
		t.Fatalf("unexpected line %+v", line)
	}
	if ctl.applied.Provider != "blacksmith" || !ctl.applied.Since.Equal(now) {
		t.Fatalf("applied %+v", ctl.applied)
	}
}

func decode(t *testing.T, buf *bytes.Buffer) decisionLine {
	t.Helper()
	if bytes.Count(buf.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("want one line, got %q", buf.String())
	}
	var line decisionLine
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	return line
}
