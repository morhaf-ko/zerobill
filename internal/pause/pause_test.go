package pause

import (
	"path/filepath"
	"testing"
	"time"
)

func TestActive(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "paused.json")
	if s, err := Load(path); err != nil || len(s.Active(now)) != 0 {
		t.Fatalf("missing file: %v %v", s, err)
	}
	if err := (Set{"runner": {}, "github": now.Add(time.Hour), "blacksmith": now.Add(-time.Hour)}).Save(path); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	active := s.Active(now)
	if len(active) != 2 || active["runner"] != "paused" || active["github"] != "paused until 2026-11-07T13:00:00Z" {
		t.Fatalf("active %v", active)
	}
}
