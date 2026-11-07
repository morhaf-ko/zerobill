package collect

import (
	"encoding/json"
	"errors"
	"os"

	"github.com/morhaf-ko/zerobill/internal/atomicfile"
)

// stateVersion changes whenever the cache layout does; an older cache is rebuilt.
const stateVersion = 2

// state caches this month's (UTC) completed jobs.
type state struct {
	Version int    `json:"version"`
	Month   string `json:"month"`
	// Jobs maps a completed job id to its runner labels and wall-clock minutes,
	// rounded up. Jobs that never reached a runner record zero minutes.
	Jobs map[string]jobRecord `json:"jobs"`
	// Runs marks run attempts ("id/attempt") whose jobs are all completed and cached.
	Runs map[string]bool `json:"runs"`
}

type jobRecord struct {
	Labels  []string `json:"l,omitempty"`
	Minutes float64  `json:"m"`
}

// loadState returns the cache for month; a missing, older or unreadable cache
// starts empty, since every entry can be fetched again.
func loadState(path, month string) (*state, error) {
	fresh := &state{Version: stateVersion, Month: month, Jobs: map[string]jobRecord{}, Runs: map[string]bool{}}
	if path == "" {
		return fresh, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fresh, nil
	}
	if err != nil {
		return nil, err
	}
	var s state
	if json.Unmarshal(raw, &s) != nil || s.Version != stateVersion || s.Month != month {
		return fresh, nil
	}
	if s.Jobs == nil {
		s.Jobs = map[string]jobRecord{}
	}
	if s.Runs == nil {
		s.Runs = map[string]bool{}
	}
	return &s, nil
}

func (s *state) save(path string) error {
	if path == "" {
		return nil
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, raw, 0o600)
}
