package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

func TestLogDecisionWritesOneJSONLine(t *testing.T) {
	cfg, providers, err := loadConfig("../../examples/zerobill.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r := collect.Result{
		States: map[string]policy.State{"blacksmith": {Online: true, UsedMinutes: 1240}, "github": {Online: true, UsedMinutes: 310}},
		Errors: map[string]string{"home": "GET /orgs/your-org/actions/runners: 403 Forbidden"},
	}
	var buf bytes.Buffer
	if err := logDecision(&buf, time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC), cfg, providers, r); err != nil {
		t.Fatal(err)
	}
	if bytes.Count(buf.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("want one line, got %q", buf.String())
	}
	var line decisionLine
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line.Provider != "blacksmith" || line.Label != "blacksmith-2vcpu-ubuntu-2404" || line.Errors["home"] == "" {
		t.Fatalf("unexpected line %+v", line)
	}
	if *line.Observed["github"].UsedMinutes != 310 || line.Observed["home"].Online != nil {
		t.Fatalf("unexpected observations %+v", line.Observed)
	}
}
