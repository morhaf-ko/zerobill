package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/morhaf-ko/zerobill/internal/config"
	"github.com/morhaf-ko/zerobill/internal/github"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

const ciWorkflow = `jobs:
  test:
    runs-on: ${{ vars.CI_RUNNER }}
  lint:
    runs-on: ubuntu-latest
  bot:
    runs-on: ${{ github.actor == 'renovate[bot]' && vars.BOT_RUNNER || vars.CI_RUNNER }}
`

// fakeGitHub serves an organization "acme" with one private repository whose
// workflow reads CI_RUNNER and BOT_RUNNER, and one runner labelled home-ci.
func fakeGitHub(t *testing.T, denied ...string) *github.Client {
	t.Helper()
	routes := map[string]string{
		"/user":                                      `{"login":"ada","plan":{"name":"free"}}`,
		"/users/acme":                                `{"type":"Organization"}`,
		"/orgs/acme":                                 `{"plan":{"name":"team"}}`,
		"/orgs/acme/repos":                           `[{"full_name":"acme/app","private":true,"archived":false,"owner":{"login":"acme"}},{"full_name":"acme/site","private":true,"archived":false,"owner":{"login":"acme"}}]`,
		"/orgs/acme/actions/runners":                 `{"runners":[{"status":"online","labels":[{"name":"self-hosted"},{"name":"Linux"},{"name":"home-ci"}]},{"status":"offline","labels":[{"name":"gpu"},{"name":"runner-pool-id=1"}]}]}`,
		"/repos/acme/app/actions/runs":               `{"workflow_runs":[]}`,
		"/repos/acme/app/actions/variables":          `{"variables":[{"name":"CI_RUNNER","value":"home-ci"},{"name":"BOT_RUNNER","value":"blacksmith-2vcpu-ubuntu-2404"}]}`,
		"/repos/acme/app/contents/.github/workflows": `[{"name":"ci.yml","path":".github/workflows/ci.yml","type":"file"}]`,
		"/repos/acme/app/contents/.github/workflows/ci.yml": fmt.Sprintf(`{"encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte(ciWorkflow))),
		"/organizations/acme/settings/billing/usage":        `{"usageItems":[]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, d := range denied {
			if r.URL.Path == d {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"message":"Resource not accessible by personal access token"}`)
				return
			}
		}
		body, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	gh := github.New("t")
	gh.BaseURL = srv.URL
	return gh
}

func TestDetectDraftsFromWorkflowsAndRunners(t *testing.T) {
	d, notes, err := detect(fakeGitHub(t), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Repos) != 1 || d.Repos[0] != "acme/app" || d.SelfHosted != "home-ci" || d.OnDemand {
		t.Fatalf("draft %+v", d)
	}
	if d.Blacksmith != "blacksmith-2vcpu-ubuntu-2404" || d.GitHub != "ubuntu-latest" || d.GitHubMinutes != 3000 {
		t.Fatalf("draft providers %+v", d)
	}
	if strings.Join(d.Vars, ",") != "CI_RUNNER" || strings.Join(d.BotVars, ",") != "BOT_RUNNER" || len(notes) != 0 {
		t.Fatalf("draft vars %+v notes %v", d, notes)
	}
}

func TestDoctor(t *testing.T) {
	cfg := &config.Config{
		Mode: config.DryRun, Owner: "acme", Repositories: []string{"acme/app"},
		Routes: []config.Route{{Name: "ci", Variables: []string{"CI_RUNNER"}}, {Name: "bot", Variables: []string{"BOT_RUNNER"}}},
	}
	providers := []policy.Provider{
		{Name: "home", Label: "home-ci", Kind: policy.SelfHosted},
		{Name: "github", Label: "ubuntu-24.04", Kind: policy.Metered},
	}
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)

	var out bytes.Buffer
	c := &checks{w: &out}
	runDoctor(c, fakeGitHub(t), cfg, providers, now)
	got := out.String()
	if c.fail != 0 || c.warned != 1 || !strings.Contains(got, "ci.yml:5 runs-on: ubuntu-latest is fixed; use ${{ vars.CI_RUNNER }}") ||
		!strings.Contains(got, "1 runner(s) with label home-ci, 1 online") || !strings.Contains(got, "2 of 3 jobs are routed") {
		t.Fatalf("fail=%d warn=%d\n%s", c.fail, c.warned, got)
	}
	if strings.Contains(got, "personal-access-tokens") {
		t.Fatalf("token link printed without a permission failure:\n%s", got)
	}

	out.Reset()
	c = &checks{w: &out}
	runDoctor(c, fakeGitHub(t, "/organizations/acme/settings/billing/usage", "/orgs/acme/actions/runners"), cfg, providers, now)
	got = out.String()
	if c.fail != 2 || !strings.Contains(got, "Organization permission Administration: read") ||
		!strings.Contains(got, "Organization permission Self-hosted runners: read") || !strings.Contains(got, "target_name=acme") {
		t.Fatalf("fail=%d\n%s", c.fail, got)
	}
}

func TestStatusShowsBudgetPaceAndSavings(t *testing.T) {
	cfg, providers, err := loadConfig("../../examples/zerobill.yaml")
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]policy.State{
		"home":       {Online: true, UsedMinutes: 500},
		"blacksmith": {Online: true, UsedMinutes: 1200},
		"github":     {Online: true, UsedMinutes: 200},
	}
	// 6 of 30 days elapsed: Blacksmith's 1200 minutes project to 6000, past its
	// 2700 limit on 14 Nov; GitHub's 200 project to 1000, inside its 1800.
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	if err := renderStatus(&out, now, cfg, providers, cfg.PolicyRoutes(providers), states, nil, nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"20% of the month", "6000, reserve on 14 Nov", "1500", "1000", "ci", "home (home-ci)", "about $3.00"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}
