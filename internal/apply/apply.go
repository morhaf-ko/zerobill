// Package apply writes the chosen runner label to repository variables.
package apply

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/morhaf-ko/zerobill/internal/atomicfile"
	"github.com/morhaf-ko/zerobill/internal/github"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

// Writer sets repository variables through the GitHub REST API.
type Writer struct {
	GH *github.Client
}

// Sync sets each variable in each repository, given as owner/name, to label.
// Variables that already hold label are not written. It returns the variables it
// wrote as "owner/name VARIABLE" and every failure joined.
func (w *Writer) Sync(repos, vars []string, label string) ([]string, error) {
	var written []string
	var errs []error
	for _, repo := range repos {
		for _, name := range vars {
			changed, err := w.set(repo, name, label)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if changed {
				written = append(written, repo+" "+name)
			}
		}
	}
	return written, errors.Join(errs...)
}

func (w *Writer) set(repo, name, label string) (bool, error) {
	base := "/repos/" + repo + "/actions/variables"
	path := base + "/" + url.PathEscape(name)
	var cur struct {
		Value string `json:"value"`
	}
	body := map[string]string{"name": name, "value": label}
	err := w.GH.Get(path, &cur)
	switch {
	case github.IsNotFound(err):
		return true, w.GH.Send(http.MethodPost, base, body)
	case err != nil:
		return false, err
	case cur.Value == label:
		return false, nil
	default:
		return true, w.GH.Send(http.MethodPatch, path, body)
	}
}

// LoadApplied reads the last applied label; a missing file means none.
func LoadApplied(path string) (policy.Applied, error) {
	var a policy.Applied
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("%s: %w", path, err)
	}
	return a, nil
}

// SaveApplied records the applied label.
func SaveApplied(path string, a policy.Applied) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, raw)
}
