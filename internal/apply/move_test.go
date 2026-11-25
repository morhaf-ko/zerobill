package apply

import (
	"encoding/base64"
	"encoding/json"
	"errors"
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
)

const fixtureSHA = "0123456789012345678901234567890123456789"

var moveTime = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
var moveRun = collect.Run{Repo: "acme/app", ID: 1, Attempt: 1, Label: "home-ci"}
var moveTargets = Targets{"acme/app": {"CI_RUNNER": "hosted-ci"}}

func noop() error { return nil }

type moveFixture struct {
	run                                            remoteRun
	workflow                                       string
	jobs                                           map[int][]recoveryJob
	posts                                          []string
	cancelCompletes, forceCompletes, rerunAdvances bool
	postError                                      int
	readError                                      int
	jobError                                       int
	paginated                                      bool
	onJobs                                         func()
	beforePost                                     func(string)
	writer                                         *Writer
}

func queuedJob() recoveryJob {
	return recoveryJob{ID: 10, Name: "build", Status: "queued", Labels: []string{"home-ci"}}
}

func newMoveFixture(t *testing.T) *moveFixture {
	t.Helper()
	f := &moveFixture{workflow: "jobs:\n  build:\n    runs-on: ${{ vars.CI_RUNNER }}\n", run: remoteRun{Status: "queued", Attempt: 1}, jobs: map[int][]recoveryJob{1: {queuedJob()}}, forceCompletes: true, rerunAdvances: true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			switch r.URL.Path {
			case "/repos/acme/app/contents/.github/workflows/ci.yml":
				_ = json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(f.workflow))})
				return
			case "/repos/acme/app/actions/variables/CI_RUNNER":
				_ = json.NewEncoder(w).Encode(map[string]string{"value": "hosted-ci"})
				return
			}
			if r.URL.Path == runPath(moveRun) {
				if f.readError != 0 {
					w.WriteHeader(f.readError)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": f.run.Status, "conclusion": f.run.Conclusion, "run_attempt": f.run.Attempt,
					"path": ".github/workflows/ci.yml", "event": "push", "head_sha": fixtureSHA,
				})
				return
			}
			var attempt int
			if _, err := fmt.Sscanf(r.URL.Path, runPath(moveRun)+"/attempts/%d/jobs", &attempt); err != nil {
				t.Errorf("unexpected GET %s", r.URL.Path)
				w.WriteHeader(404)
				return
			}
			if f.jobError != 0 {
				w.WriteHeader(f.jobError)
				return
			}
			jobs := f.jobs[attempt]
			if f.paginated && len(jobs) > 1 {
				if r.URL.Query().Get("page") == "" {
					w.Header().Set("Link", "<http://"+r.Host+r.URL.Path+"?page=2>; rel=\"next\"")
					jobs = jobs[:1]
				} else {
					jobs = jobs[1:]
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(f.jobs[attempt]), "jobs": jobs})
			if f.onJobs != nil {
				f.onJobs()
			}
			return
		}
		action := strings.TrimPrefix(r.URL.Path, runPath(moveRun)+"/")
		f.posts = append(f.posts, action)
		if f.beforePost != nil {
			f.beforePost(action)
		}
		if f.postError != 0 {
			w.WriteHeader(f.postError)
			return
		}
		switch action {
		case "cancel":
			if f.cancelCompletes {
				f.complete()
			}
		case "force-cancel":
			if f.forceCompletes {
				f.complete()
			}
		case "rerun-failed-jobs":
			if f.rerunAdvances {
				f.run.Attempt++
				f.run.Status, f.run.Conclusion = "queued", ""
				f.jobs[f.run.Attempt] = []recoveryJob{{ID: 20, Name: "build", Status: "in_progress", Labels: []string{"hosted-ci"}, Runner: "hosted"}}
			}
		default:
			t.Errorf("unexpected POST %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	gh := github.New("fixture-token")
	gh.BaseURL = srv.URL
	f.writer = &Writer{GH: gh}
	return f
}

func (f *moveFixture) complete() {
	f.run.Status, f.run.Conclusion = "completed", "cancelled"
	for i := range f.jobs[f.run.Attempt] {
		j := &f.jobs[f.run.Attempt][i]
		if j.Status == "queued" {
			j.Status, j.Conclusion = "completed", "cancelled"
		}
	}
}

func (f *moveFixture) move(t *testing.T, st *State, at time.Duration, stuck ...collect.Run) Moves {
	t.Helper()
	out, err := f.writer.Move(st, stuck, moveTargets, moveTime.Add(at), noop)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func ownedCancellation() State {
	return State{Cancelled: map[string]Moved{key(moveRun): {Run: moveRun, At: moveTime, Jobs: []MigratedJob{{ID: 10, Name: "build", Variable: "CI_RUNNER", Label: "hosted-ci"}}}}}
}

func TestMoveRecoversAcceptedCancellationThatStaysQueued(t *testing.T) {
	f := newMoveFixture(t)
	st := State{}
	if got := f.move(t, &st, 0, moveRun); len(got.Cancelled) != 1 {
		t.Fatalf("cancel: %+v", got)
	}
	f.move(t, &st, 4*time.Minute, moveRun)
	if !slices.Equal(f.posts, []string{"cancel"}) {
		t.Fatal(f.posts)
	}
	if got := f.move(t, &st, 5*time.Minute, moveRun); len(got.Forced) != 1 {
		t.Fatalf("force: %+v", got)
	}
	if got := f.move(t, &st, 6*time.Minute, moveRun); len(got.Rerun) != 0 {
		t.Fatal("accepted rerun was reported as verified")
	}
	if got := f.move(t, &st, 7*time.Minute, moveRun); len(got.Rerun) != 1 || len(st.Cancelled) != 0 {
		t.Fatalf("verify: %+v %+v", got, st)
	}
	f.move(t, &st, 8*time.Minute, moveRun)
	if !slices.Equal(f.posts, []string{"cancel", "force-cancel", "rerun-failed-jobs"}) {
		t.Fatal(f.posts)
	}
}

func TestMoveNormalCancellationNeedsNoForce(t *testing.T) {
	f := newMoveFixture(t)
	f.cancelCompletes = true
	st := State{}
	f.move(t, &st, 0, moveRun)
	f.move(t, &st, time.Minute)
	got := f.move(t, &st, 2*time.Minute)
	if len(got.Rerun) != 1 || !slices.Equal(f.posts, []string{"cancel", "rerun-failed-jobs"}) {
		t.Fatalf("%+v %v", got, f.posts)
	}
}

func TestMoveRetainsUnresolvedRecoveryWithoutReenrolling(t *testing.T) {
	f := newMoveFixture(t)
	f.forceCompletes = false
	st := ownedCancellation()
	f.move(t, &st, 5*time.Minute, moveRun)
	f.move(t, &st, 10*time.Minute, moveRun)
	f.move(t, &st, 2*time.Hour, moveRun)
	if !st.Cancelled[key(moveRun)].NeedsAttention() || !slices.Equal(f.posts, []string{"force-cancel"}) {
		t.Fatalf("%+v %v", st, f.posts)
	}
}

func TestMoveForceNeverInterruptsExecutedOrUnknownJobs(t *testing.T) {
	cases := map[string][]recoveryJob{
		"running":          {{ID: 10, Status: "in_progress", Runner: "runner"}},
		"successful work":  {queuedJob(), {ID: 11, Status: "completed", Conclusion: "success", Runner: "runner"}},
		"runner assigned":  {{ID: 10, Name: "build", Status: "queued", Labels: []string{"home-ci"}, RunnerID: 22}},
		"failed work":      {queuedJob(), {ID: 11, Status: "completed", Conclusion: "failure"}},
		"no jobs":          {},
		"missing id":       {{Status: "queued"}},
		"pending approval": {{ID: 10, Status: "waiting"}},
	}
	for name, jobs := range cases {
		t.Run(name, func(t *testing.T) {
			f := newMoveFixture(t)
			f.jobs[1] = jobs
			st := ownedCancellation()
			f.move(t, &st, 5*time.Minute, moveRun)
			if len(f.posts) != 0 || !st.Cancelled[key(moveRun)].NeedsAttention() {
				t.Fatalf("%+v %v", st, f.posts)
			}
		})
	}
}

func TestQueuedStartedAtDoesNotImplyExecution(t *testing.T) {
	var job recoveryJob
	if err := json.Unmarshal([]byte("{\"id\":10,\"status\":\"queued\",\"started_at\":\"2026-01-01T10:00:00Z\",\"runner_id\":0,\"runner_name\":\"\",\"steps\":[]}"), &job); err != nil {
		t.Fatal(err)
	}
	if !unstarted(job) {
		t.Fatal("queue timestamp was mistaken for executed work")
	}
}

func TestMoveChecksEveryJobPageAndStartedStep(t *testing.T) {
	f := newMoveFixture(t)
	f.paginated = true
	j := queuedJob()
	j.ID = 11
	j.Name = "second"
	j.Steps = append(j.Steps, recoveryStep{Status: "in_progress"})
	f.jobs[1] = append(f.jobs[1], j)
	st := ownedCancellation()
	f.move(t, &st, 5*time.Minute)
	if len(f.posts) != 0 || !st.Cancelled[key(moveRun)].NeedsAttention() {
		t.Fatal("started step on page2 was missed")
	}
}

func TestMoveRechecksAttemptAfterReadingJobs(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint(pending), func(t *testing.T) {
			f := newMoveFixture(t)
			f.onJobs = func() { f.run.Attempt++ }
			st := State{}
			if pending {
				st = ownedCancellation()
			}
			f.move(t, &st, 5*time.Minute, moveRun)
			if len(f.posts) != 0 {
				t.Fatal(f.posts)
			}
		})
	}
}

func TestMoveNormalCancelRevalidatesWorkAndLabels(t *testing.T) {
	for _, j := range []recoveryJob{{ID: 10, Status: "in_progress", Runner: "r"}, {ID: 10, Name: "build", Status: "queued", Labels: []string{"other"}}} {
		f := newMoveFixture(t)
		f.jobs[1] = []recoveryJob{j}
		st := State{}
		f.move(t, &st, 0, moveRun)
		if len(f.posts) != 0 {
			t.Fatal(f.posts)
		}
	}
}

func TestMoveChecksRerunLabelsAndExternalAttempts(t *testing.T) {
	t.Run("wrong label", func(t *testing.T) {
		f := newMoveFixture(t)
		f.complete()
		st := ownedCancellation()
		f.move(t, &st, time.Minute)
		f.jobs[2][0].Labels = []string{"home-ci"}
		f.move(t, &st, 2*time.Minute, moveRun)
		f.move(t, &st, 3*time.Minute, moveRun)
		if !st.Cancelled[key(moveRun)].NeedsAttention() || st.Cancelled[key(moveRun)].Attempt != 2 {
			t.Fatal(st)
		}
		f.run.Attempt = 3
		f.move(t, &st, 4*time.Minute)
		if len(st.Cancelled) != 0 || len(f.posts) != 1 {
			t.Fatalf("%+v %v", st, f.posts)
		}
	})
	t.Run("external rerun", func(t *testing.T) {
		f := newMoveFixture(t)
		f.run.Attempt = 2
		st := ownedCancellation()
		f.move(t, &st, time.Minute, moveRun)
		if len(f.posts) != 0 || len(st.Cancelled) != 0 || len(st.Rerun) != 1 {
			t.Fatalf("%+v %v", st, f.posts)
		}
	})
	t.Run("accepted without new attempt", func(t *testing.T) {
		f := newMoveFixture(t)
		f.complete()
		f.rerunAdvances = false
		st := ownedCancellation()
		f.move(t, &st, time.Minute)
		f.move(t, &st, 6*time.Minute, moveRun)
		f.move(t, &st, 2*time.Hour, moveRun)
		if !st.Cancelled[key(moveRun)].NeedsAttention() || len(f.posts) != 1 {
			t.Fatalf("%+v %v", st, f.posts)
		}
	})
}

func TestMovePersistsIntentBeforeEveryPostAndSurvivesRestart(t *testing.T) {
	f := newMoveFixture(t)
	st := State{}
	path := filepath.Join(t.TempDir(), "state.json")
	checkpoint := func() error { return SaveState(path, st) }
	f.beforePost = func(action string) {
		s, err := LoadState(path)
		if err != nil {
			t.Error(err)
			return
		}
		stage := s.Cancelled[key(moveRun)].Stage
		if want := map[string]string{"cancel": cancelRequested, "force-cancel": forceRequested, "rerun-failed-jobs": rerunRequested}[action]; stage != want {
			t.Errorf("%s: stage %q", action, stage)
		}
	}
	for _, at := range []time.Duration{0, 5 * time.Minute, 6 * time.Minute, 7 * time.Minute} {
		if _, err := f.writer.Move(&st, []collect.Run{moveRun}, moveTargets, moveTime.Add(at), checkpoint); err != nil {
			t.Fatal(err)
		}
		var err error
		st, err = LoadState(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(f.posts, []string{"cancel", "force-cancel", "rerun-failed-jobs"}) || len(st.Rerun) != 1 {
		t.Fatalf("%+v %v", st, f.posts)
	}
}

func TestMoveRestartNeverRepeatsUnconfirmedPost(t *testing.T) {
	for _, stage := range []string{cancelRequested, forceRequested, rerunRequested} {
		t.Run(stage, func(t *testing.T) {
			f := newMoveFixture(t)
			f.forceCompletes = false
			st := ownedCancellation()
			m := st.Cancelled[key(moveRun)]
			m.Stage = stage
			st.Cancelled[key(moveRun)] = m
			f.move(t, &st, 6*time.Minute, moveRun)
			if len(f.posts) != 0 || !st.Cancelled[key(moveRun)].NeedsAttention() {
				t.Fatalf("%+v %v", st, f.posts)
			}
		})
	}
}

func TestMoveCheckpointFailurePreventsMutation(t *testing.T) {
	for _, action := range []string{"cancel", "force-cancel", "rerun-failed-jobs"} {
		t.Run(action, func(t *testing.T) {
			f := newMoveFixture(t)
			st := State{}
			if action != "cancel" {
				st = ownedCancellation()
			}
			if action == "rerun-failed-jobs" {
				f.complete()
			}
			_, err := f.writer.Move(&st, []collect.Run{moveRun}, moveTargets, moveTime.Add(5*time.Minute), func() error { return errors.New("disk full") })
			if err == nil || len(f.posts) != 0 || st.Cancelled[key(moveRun)].Stage != "" {
				t.Fatalf("%+v %v %v", st, err, f.posts)
			}
			f.move(t, &st, 6*time.Minute, moveRun)
			if !slices.Equal(f.posts, []string{action}) {
				t.Fatalf("request not issued after storage recovered: %v", f.posts)
			}
		})
	}
}

func TestMoveAPIErrorsDoNotRepeatWrites(t *testing.T) {
	for _, code := range []int{403, 409, 429, 502} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f := newMoveFixture(t)
			f.postError = code
			st := ownedCancellation()
			f.move(t, &st, 5*time.Minute, moveRun)
			f.move(t, &st, 6*time.Minute, moveRun)
			if len(f.posts) != 1 || !st.Cancelled[key(moveRun)].NeedsAttention() {
				t.Fatalf("%+v %v", st, f.posts)
			}
		})
	}
}

func TestMoveReadErrorsBecomeAttentionWithoutDroppingRecord(t *testing.T) {
	for _, jobRead := range []bool{false, true} {
		t.Run(fmt.Sprint(jobRead), func(t *testing.T) {
			f := newMoveFixture(t)
			if jobRead {
				f.jobError = 502
			} else {
				f.readError = 502
			}
			st := ownedCancellation()
			_, err := f.writer.Move(&st, nil, moveTargets, moveTime.Add(6*time.Minute), noop)
			if err == nil || !st.Cancelled[key(moveRun)].NeedsAttention() || len(f.posts) != 0 {
				t.Fatalf("%+v %v", st, err)
			}
		})
	}
}

func TestMoveAttentionClearsAfterExternalCompletion(t *testing.T) {
	f := newMoveFixture(t)
	st := ownedCancellation()
	m := st.Cancelled[key(moveRun)]
	m.Stage = needsAttention
	m.Problem = "stalled"
	st.Cancelled[key(moveRun)] = m
	f.complete()
	f.run.Conclusion = "success"
	f.move(t, &st, time.Minute, moveRun)
	if len(st.Cancelled) != 0 || len(st.Rerun) != 1 || len(f.posts) != 0 {
		t.Fatal(st)
	}
	f.move(t, &st, 49*time.Hour)
	if len(st.Rerun) != 0 {
		t.Fatal("terminal history was not pruned")
	}
}

func TestMoveRejectsStaleQueueAndUnusableRoutes(t *testing.T) {
	for _, status := range []string{"waiting", "pending", "requested"} {
		t.Run(status, func(t *testing.T) {
			f := newMoveFixture(t)
			f.run.Status = status
			st := State{}
			f.move(t, &st, 0, moveRun)
			if len(f.posts) != 0 {
				t.Fatalf("stale queue triggered %v", f.posts)
			}
		})
	}
	for _, targets := range []Targets{nil, {"acme/app": {"CI_RUNNER": "home-ci"}}, {"acme/app": {"CI_RUNNER": "zerobill-wait"}}} {
		t.Run(fmt.Sprint(targets), func(t *testing.T) {
			f := newMoveFixture(t)
			st := State{}
			_, err := f.writer.Move(&st, []collect.Run{moveRun}, targets, moveTime, noop)
			if err != nil || len(f.posts) != 0 {
				t.Fatalf("unusable route triggered %v: %v", f.posts, err)
			}
		})
	}
}

func TestMoveDoesNotFollowChangedRouteAfterCancellation(t *testing.T) {
	f := newMoveFixture(t)
	f.cancelCompletes = true
	st := State{}
	f.move(t, &st, 0, moveRun)
	_, err := f.writer.Move(&st, nil, Targets{"acme/app": {"CI_RUNNER": "other-hosted"}}, moveTime.Add(time.Minute), noop)
	if err != nil || !slices.Equal(f.posts, []string{"cancel"}) {
		t.Fatalf("changed route triggered %v: %v", f.posts, err)
	}
}

func TestMoveMatchesEachQueuedJobToItsOwnRoute(t *testing.T) {
	f := newMoveFixture(t)
	f.workflow = "jobs:\n  build:\n    runs-on: ${{ vars.CI_RUNNER }}\n  bot:\n    runs-on: ${{ vars.BOT_RUNNER }}\n  fixed:\n    runs-on: home-ci\n"
	for _, name := range []string{"bot", "fixed"} {
		j := queuedJob()
		j.ID, j.Name = int64(len(f.jobs[1])+10), name
		f.jobs[1] = append(f.jobs[1], j)
	}
	st := State{}
	f.move(t, &st, 0, moveRun)
	if len(f.posts) != 0 {
		t.Fatalf("mixed routes triggered %v", f.posts)
	}
}

func TestMoveVerifiesMigratedJobRatherThanVisibleSibling(t *testing.T) {
	for _, tc := range []struct {
		name      string
		jobs      []recoveryJob
		runStatus string
		confirmed bool
		report    string
		attention bool
	}{
		{"partial", []recoveryJob{{ID: 21, Name: "downstream", Status: "queued", Labels: []string{"hosted-ci"}}}, "queued", false, "build", false},
		{"wrong", []recoveryJob{{ID: 21, Name: "downstream", Status: "queued", Labels: []string{"hosted-ci"}}, {ID: 20, Name: "build", Status: "queued", Labels: []string{"other"}}}, "queued", false, "unexpected", true},
		{"missing labels", []recoveryJob{{ID: 20, Name: "build", Status: "queued"}}, "queued", false, "build", false},
		{"waiting approval", []recoveryJob{{ID: 20, Name: "build", Status: "waiting", Labels: []string{"hosted-ci"}}}, "waiting", true, "GitHub", false},
		{"pending concurrency", []recoveryJob{{ID: 20, Name: "build", Status: "pending", Labels: []string{"hosted-ci"}}}, "pending", true, "GitHub", false},
		{"skipped", []recoveryJob{{ID: 20, Name: "build", Status: "completed", Conclusion: "skipped", Labels: []string{"hosted-ci"}}, {ID: 21, Name: "downstream", Status: "queued", Labels: []string{"hosted-ci"}}}, "completed", false, "skipped", true},
		{"assigned", []recoveryJob{{ID: 20, Name: "build", Status: "queued", Labels: []string{"hosted-ci"}, RunnerID: 3}}, "queued", true, "assigned", false},
		{"started", []recoveryJob{{ID: 20, Name: "build", Status: "in_progress", Labels: []string{"hosted-ci"}, Runner: "hosted"}, {ID: 21, Name: "downstream", Status: "queued", Labels: []string{"fixed"}}}, "in_progress", true, "started", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMoveFixture(t)
			f.complete()
			st := ownedCancellation()
			f.move(t, &st, time.Minute)
			f.run.Status = tc.runStatus
			f.jobs[2] = tc.jobs
			got := f.move(t, &st, 2*time.Minute)
			k := key(moveRun)
			m := st.Cancelled[k]
			if (len(got.Rerun) == 1) != tc.confirmed || m.NeedsAttention() != tc.attention ||
				!strings.Contains(got.Recovery[k]+m.Problem, tc.report) || len(f.posts) != 1 {
				t.Fatalf("got=%+v state=%+v posts=%v", got, m, f.posts)
			}
		})
	}
}

func TestMovePreservesCompletedSiblingsDuringForceAndRerun(t *testing.T) {
	f := newMoveFixture(t)
	f.jobs[1] = append(f.jobs[1], recoveryJob{ID: 11, Name: "lint", Status: "completed", Conclusion: "success", Runner: "lint-runner"})
	st := State{}
	for _, at := range []time.Duration{0, 5 * time.Minute, 6 * time.Minute, 7 * time.Minute} {
		f.move(t, &st, at, moveRun)
	}
	if !slices.Equal(f.posts, []string{"cancel", "force-cancel", "rerun-failed-jobs"}) || len(st.Rerun) != 1 {
		t.Fatalf("posts=%v state=%+v", f.posts, st)
	}
}

func TestMoveLabelConfirmationIsDurableWhilePickupIsUnverified(t *testing.T) {
	f := newMoveFixture(t)
	f.complete()
	st := ownedCancellation()
	path := filepath.Join(t.TempDir(), "applied.json")
	for _, at := range []time.Duration{time.Minute, 2 * time.Minute, 3 * time.Minute, 6 * time.Minute} {
		got, err := f.writer.Move(&st, nil, moveTargets, moveTime.Add(at), func() error { return SaveState(path, st) })
		if err != nil {
			t.Fatal(err)
		}
		if (len(got.Rerun) == 1) != (at == 2*time.Minute) {
			t.Fatalf("at %s: %+v", at, got)
		}
		st, err = LoadState(path)
		if err != nil {
			t.Fatal(err)
		}
		f.jobs[2][0].Status = "queued"
		f.jobs[2][0].Runner = ""
	}
	m := st.Cancelled[key(moveRun)]
	if !m.NeedsAttention() || !m.LabelsConfirmed || m.Attempt != 2 || len(f.posts) != 1 {
		t.Fatalf("posts=%v state=%+v", f.posts, st)
	}
}

func TestMoveLegacyCancellationKeepsOwnershipWithoutPosting(t *testing.T) {
	f := newMoveFixture(t)
	f.complete()
	var st State
	if err := json.Unmarshal([]byte(`{"cancelled":{"acme/app#1":{"repo":"acme/app","id":1,"attempt":1,"label":"home-ci","at":"2026-01-01T12:00:00Z","expected":["hosted-ci"]}}}`), &st); err != nil {
		t.Fatal(err)
	}
	f.move(t, &st, time.Minute, moveRun)
	if m := st.Cancelled[key(moveRun)]; len(f.posts) != 0 || !m.NeedsAttention() || !strings.Contains(m.Problem, "no job and replacement route evidence") {
		t.Fatalf("posts=%v record=%+v", f.posts, m)
	}
}

func TestMoveLeavesDependentJobsToOperator(t *testing.T) {
	f := newMoveFixture(t)
	f.workflow += "  publish:\n    needs: build\n    runs-on: paid-fixed-runner\n"
	st := State{}
	got := f.move(t, &st, 0, moveRun)
	if len(f.posts) != 0 || !strings.Contains(got.Recovery[key(moveRun)], "depends on") {
		t.Fatalf("posts=%v recovery=%v", f.posts, got.Recovery)
	}
}
