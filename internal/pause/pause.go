// Package pause keeps providers the user took out of rotation.
package pause

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/morhaf-ko/zerobill/internal/atomicfile"
)

// Set maps a provider to the time its pause ends; a zero time pauses it until resumed.
type Set map[string]time.Time

// Load reads pauses; a missing file means none.
func Load(path string) (Set, error) {
	s := Set{}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return Set{}, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Save writes pauses atomically, so a running zerobill never reads a partial file.
func (s Set) Save(path string) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, raw)
}

// Active returns why each provider is paused at now.
func (s Set) Active(now time.Time) map[string]string {
	out := map[string]string{}
	for name, until := range s {
		switch {
		case until.IsZero():
			out[name] = "paused"
		case now.Before(until):
			out[name] = "paused until " + until.UTC().Format(time.RFC3339)
		}
	}
	return out
}
