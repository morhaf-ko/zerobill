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

	"github.com/morhaf-ko/zerobill/internal/collect"
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

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "applied.json")
	if st, err := LoadState(path); err != nil || st.Label != "" {
		t.Fatalf("missing file: %+v %v", st, err)
	}
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	want := State{
		Applied:   policy.Applied{Provider: "home", Label: "home-ci", Since: at},
		Cancelled: map[string]Moved{"acme/app#1": {Run: collect.Run{Repo: "acme/app", ID: 1, Attempt: 1}, At: at}},
	}
	if err := SaveState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil || got.Applied != want.Applied || got.Cancelled["acme/app#1"] != want.Cancelled["acme/app#1"] {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestMoveCancelsThenReruns(t *testing.T) {
	status := map[string]string{"1": "queued", "2": "queued", "3": "completed"}
	var posts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		id := parts[6]
		if r.Method == http.MethodGet {
			attempt := 1
			if id == "4" {
				attempt = 2
			}
			fmt.Fprintf(w, `{"status":%q,"run_attempt":%d}`, status[id], attempt)
			return
		}
		posts = append(posts, id+" "+parts[7])
		switch {
		case parts[7] == "cancel" && status[id] == "completed":
			w.WriteHeader(http.StatusConflict)
		case parts[7] == "cancel":
			status[id] = "completed"
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()
	gh := github.New("t")
	gh.BaseURL = srv.URL
	w := &Writer{GH: gh}
	t0 := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	run := func(id int64) collect.Run { return collect.Run{Repo: "acme/app", ID: id, Attempt: 1} }
	st := State{Cancelled: map[string]Moved{"acme/app#4": {Run: run(4), At: t0}}}

	cancelled, rerun, err := w.Move(&st, []collect.Run{run(1), run(2), run(3)}, t0, noop)
	if err != nil {
		t.Fatal(err)
	}
	// Run 3 finished before the cancel; run 4 was rerun by someone else. Both count as moved.
	if !slices.Equal(cancelled, []string{"acme/app#1", "acme/app#2"}) || len(rerun) != 0 || len(st.Cancelled) != 2 || len(st.Rerun) != 2 {
		t.Fatalf("first interval cancelled %v rerun %v state %+v", cancelled, rerun, st.Cancelled)
	}

	posts = nil
	cancelled, rerun, err = w.Move(&st, []collect.Run{run(1)}, t0.Add(5*time.Minute), noop)
	if err != nil {
		t.Fatal(err)
	}
	if len(cancelled) != 0 || !slices.Equal(rerun, []string{"acme/app#1", "acme/app#2"}) || len(st.Cancelled) != 0 {
		t.Fatalf("second interval cancelled %v rerun %v", cancelled, rerun)
	}
	slices.Sort(posts)
	if !slices.Equal(posts, []string{"1 rerun-failed-jobs", "2 rerun-failed-jobs"}) {
		t.Fatalf("requests %v", posts)
	}

	// A moved run that queues on the stale label again is left alone.
	posts = nil
	if cancelled, _, _ = w.Move(&st, []collect.Run{run(1)}, t0.Add(10*time.Minute), noop); len(cancelled) != 0 || len(posts) != 0 {
		t.Fatalf("moved run 1 twice: %v %v", cancelled, posts)
	}
	if _, _, _ = w.Move(&st, nil, t0.Add(49*time.Hour), noop); len(st.Rerun) != 0 {
		t.Fatalf("rerun memory not pruned: %+v", st.Rerun)
	}
}

func noop() error { return nil }

func TestMoveCheckpointsEachCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	gh := github.New("t")
	gh.BaseURL = srv.URL
	var saved []int
	st := State{}
	checkpoint := func() error { saved = append(saved, len(st.Cancelled)); return nil }
	runs := []collect.Run{{Repo: "acme/app", ID: 1, Attempt: 1}, {Repo: "acme/app", ID: 2, Attempt: 1}}
	if _, _, err := (&Writer{GH: gh}).Move(&st, runs, time.Now(), checkpoint); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(saved, []int{1, 2}) {
		t.Fatalf("checkpoints %v, want one after each cancel", saved)
	}
}

func TestMoveGivesUpOnFailingRerun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"status":"completed","run_attempt":1}`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
	}))
	defer srv.Close()
	gh := github.New("t")
	gh.BaseURL = srv.URL
	t0 := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	st := State{Cancelled: map[string]Moved{"acme/app#1": {Run: collect.Run{Repo: "acme/app", ID: 1, Attempt: 1}, At: t0}}}
	if _, _, err := (&Writer{GH: gh}).Move(&st, nil, t0.Add(5*time.Minute), noop); err == nil || len(st.Cancelled) != 1 {
		t.Fatalf("want a retryable 403, got %v %+v", err, st.Cancelled)
	}
	_, _, err := (&Writer{GH: gh}).Move(&st, nil, t0.Add(2*time.Hour), noop)
	if err == nil || !strings.Contains(err.Error(), "gave up") || !strings.Contains(err.Error(), "403") || len(st.Cancelled) != 0 {
		t.Fatalf("want a give-up error naming the 403, got %v %+v", err, st.Cancelled)
	}
}

func TestMoveGivesUpOnRunThatNeverCompletes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"in_progress","run_attempt":1}`)
	}))
	defer srv.Close()
	gh := github.New("t")
	gh.BaseURL = srv.URL
	t0 := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	st := State{Cancelled: map[string]Moved{"acme/app#1": {Run: collect.Run{Repo: "acme/app", ID: 1, Attempt: 1}, At: t0}}}
	if _, _, err := (&Writer{GH: gh}).Move(&st, nil, t0.Add(10*time.Minute), noop); err != nil || len(st.Cancelled) != 1 {
		t.Fatalf("dropped too early: %v %+v", err, st.Cancelled)
	}
	if _, _, err := (&Writer{GH: gh}).Move(&st, nil, t0.Add(2*time.Hour), noop); err == nil || len(st.Cancelled) != 0 {
		t.Fatalf("want a give-up error, got %v %+v", err, st.Cancelled)
	}
}
