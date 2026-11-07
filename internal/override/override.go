// Package override stores operator decisions that take providers out of rotation.
package override

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/morhaf-ko/zerobill/internal/atomicfile"
)

// Set maps provider names to their disable entries.
type Set struct {
	Disabled map[string]Entry `json:"disabled,omitempty"`
}

// Entry disables a provider until Until; a zero Until disables it until enabled.
type Entry struct {
	Until time.Time `json:"until,omitzero"`
	Since time.Time `json:"since"`
}

// Load reads overrides; a missing file means none.
func Load(path string) (Set, error) {
	var s Set
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Save writes overrides atomically, so a running watch never reads a partial file.
// The file is world-readable: it holds no secrets, and a service account must be
// able to read overrides an administrator wrote.
func Save(path string, s Set) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(path, append(raw, '\n'), 0o644)
}

// Active returns the reason each provider is disabled at now.
func (s Set) Active(now time.Time) map[string]string {
	out := map[string]string{}
	for name, e := range s.Disabled {
		switch {
		case e.Until.IsZero():
			out[name] = "disabled by operator"
		case now.Before(e.Until):
			out[name] = "disabled by operator until " + e.Until.UTC().Format(time.RFC3339)
		}
	}
	return out
}
