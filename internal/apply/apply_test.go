package apply

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/morhaf-ko/zerobill/internal/collect"
	"github.com/morhaf-ko/zerobill/internal/github"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

func TestSetWritesOnlyChangedVariables(t *testing.T) {
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
	var wrote []string
	for _, name := range []string{"CI_RUNNER", "RELEASE_RUNNER", "LIGHT_RUNNER"} {
		changed, err := w.Set("acme/app", name, "ubuntu-24.04")
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			wrote = append(wrote, name)
		}
	}
	if want := []string{"CI_RUNNER", "RELEASE_RUNNER"}; !slices.Equal(wrote, want) {
		t.Fatalf("written %v, want %v", wrote, want)
	}
	if want := []string{"PATCH /repos/acme/app/actions/variables/CI_RUNNER", "POST /repos/acme/app/actions/variables"}; !slices.Equal(writes, want) {
		t.Fatalf("requests %v, want %v", writes, want)
	}
	writes = nil
	if changed, err := w.Set("acme/app", "CI_RUNNER", "ubuntu-24.04"); changed || err != nil || len(writes) != 0 {
		t.Fatalf("second set wrote %v %v: %v", changed, writes, err)
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "applied.json")
	if st, err := LoadState(path); err != nil || len(st.Routes) != 0 {
		t.Fatalf("missing file: %+v %v", st, err)
	}
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	want := State{
		Routes:    map[string]policy.Applied{"default": {Provider: "home", Label: "home-ci", Since: at}},
		Stalled:   map[string]time.Time{"home": at},
		Cancelled: map[string]Moved{"acme/app#1": {Run: collect.Run{Repo: "acme/app", ID: 1, Attempt: 1}, At: at}},
	}
	if err := SaveState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil || got.Routes["default"] != want.Routes["default"] || !got.Stalled["home"].Equal(at) || !reflect.DeepEqual(got.Cancelled, want.Cancelled) {
		t.Fatalf("got %+v %v", got, err)
	}
}
