package apply

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/morhaf-ko/zerobill/internal/github"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

func TestSyncWritesOnlyChangedVariables(t *testing.T) {
	values := map[string]string{"CI_RUNNER": "home-ci", "LIGHT_RUNNER": "ubuntu-24.04"}
	var writes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const base = "/repos/acme/app/actions/variables"
		var in struct{ Name, Value string }
		if r.Method != http.MethodGet {
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("%s %s: %v", r.Method, r.URL.Path, err)
			}
			writes = append(writes, r.Method+" "+r.URL.Path)
		}
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, base+"/"):
			v, ok := values[strings.TrimPrefix(r.URL.Path, base+"/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
			fmt.Fprintf(w, `{"value":%q}`, v)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, base+"/"):
			values[in.Name] = in.Value
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == base:
			values[in.Name] = in.Value
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()
	gh := github.New("t")
	gh.BaseURL = srv.URL
	w := &Writer{GH: gh}
	vars := []string{"CI_RUNNER", "RELEASE_RUNNER", "LIGHT_RUNNER"}

	got, err := w.Sync([]string{"acme/app"}, vars, "ubuntu-24.04")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme/app CI_RUNNER", "acme/app RELEASE_RUNNER"}; !slices.Equal(got, want) {
		t.Fatalf("written %v, want %v", got, want)
	}
	if want := []string{"PATCH /repos/acme/app/actions/variables/CI_RUNNER", "POST /repos/acme/app/actions/variables"}; !slices.Equal(writes, want) {
		t.Fatalf("requests %v, want %v", writes, want)
	}

	writes = nil
	got, err = w.Sync([]string{"acme/app"}, vars, "ubuntu-24.04")
	if err != nil || len(got) != 0 || len(writes) != 0 {
		t.Fatalf("second sync wrote %v %v: %v", got, writes, err)
	}
}

func TestSyncReportsEveryFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/denied/") {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
			return
		}
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"value":"home-ci"}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	gh := github.New("t")
	gh.BaseURL = srv.URL
	got, err := (&Writer{GH: gh}).Sync([]string{"acme/denied", "acme/app"}, []string{"CI_RUNNER"}, "ubuntu-24.04")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("want a 403 error, got %v", err)
	}
	if !slices.Equal(got, []string{"acme/app CI_RUNNER"}) {
		t.Fatalf("written %v", got)
	}
}

func TestAppliedRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "applied.json")
	if a, err := LoadApplied(path); err != nil || a.Label != "" {
		t.Fatalf("missing file: %+v %v", a, err)
	}
	want := policy.Applied{Provider: "home", Label: "home-ci", Since: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	if err := SaveApplied(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadApplied(path)
	if err != nil || got != want {
		t.Fatalf("got %+v %v", got, err)
	}
}
