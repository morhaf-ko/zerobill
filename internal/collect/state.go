package collect

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/morhaf-ko/zerobill/internal/atomicfile"
)

// state caches Blacksmith minutes for one calendar month (UTC).
type state struct {
	Month string `json:"month"`
	// Jobs maps a completed job id to its billed minutes, zero for other labels.
	Jobs map[string]float64 `json:"jobs"`
	// Runs marks run attempts ("id/attempt") whose jobs are all completed and cached.
	Runs map[string]bool `json:"runs"`
}

func loadState(path, month string) (*state, error) {
	fresh := &state{Month: month, Jobs: map[string]float64{}, Runs: map[string]bool{}}
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
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.Month != month {
		return fresh, nil
	}
	if s.Jobs == nil {
		s.Jobs = map[string]float64{}
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
	return atomicfile.Write(path, raw)
}
