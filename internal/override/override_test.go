package override

import (
	"path/filepath"
	"testing"
	"time"
)

func TestActive(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "overrides.json")
	if s, err := Load(path); err != nil || len(s.Active(now)) != 0 {
		t.Fatalf("missing file: %+v %v", s, err)
	}
	want := Set{Paused: map[string]Entry{
		"home":   {Since: now},
		"github": {Since: now, Until: now.Add(time.Hour)},
		"old":    {Since: now.Add(-2 * time.Hour), Until: now.Add(-time.Hour)},
	}}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	active := got.Active(now)
	if len(active) != 2 || active["home"] != "paused" || active["github"] != "paused until 2026-11-07T13:00:00Z" {
		t.Fatalf("active %v", active)
	}
}
