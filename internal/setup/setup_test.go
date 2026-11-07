package setup_test

import (
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/morhaf-ko/zerobill/internal/config"
	"github.com/morhaf-ko/zerobill/internal/setup"
)

const workflow = `name: ci
on: [pull_request]
jobs:
  inline:
    runs-on: ubuntu-latest # comment
  variable:
    runs-on: ${{ vars.CI_RUNNER }}
  bot:
    runs-on: ${{ github.actor == 'renovate[bot]' && vars.BOT_RUNNER || vars.CI_RUNNER }}
  flow:
    runs-on: [self-hosted, home-ci]
  block:
    runs-on:
      - self-hosted
      - gpu
    steps:
      - run: echo
  group:
    runs-on:
      group: big
`

func TestParseRunsOn(t *testing.T) {
	got := setup.ParseRunsOn("ci.yml", workflow)
	want := []struct {
		line   int
		labels []string
		vars   []string
	}{
		{5, []string{"ubuntu-latest"}, nil},
		{7, nil, []string{"CI_RUNNER"}},
		{9, nil, []string{"BOT_RUNNER", "CI_RUNNER"}},
		{11, []string{"self-hosted", "home-ci"}, nil},
		{13, []string{"self-hosted", "gpu"}, nil},
		{19, nil, nil},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d declarations: %+v", len(got), got)
	}
	for i, w := range want {
		g := got[i]
		if g.Line != w.line || !slices.Equal(g.Labels, w.labels) || !slices.Equal(g.Vars, w.vars) {
			t.Errorf("declaration %d: got line %d labels %v vars %v (%q)", i, g.Line, g.Labels, g.Vars, g.Value)
		}
	}
	if got[5].Value != "group: big" {
		t.Errorf("group value %q", got[5].Value)
	}
}

func TestDraftsLoad(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	drafts := map[string]setup.Draft{
		"homelab with bots": {Owner: "acme", Repos: []string{"acme/app"}, SelfHosted: "home-ci", OnDemand: true,
			Blacksmith: "blacksmith-2vcpu-ubuntu-2404", GitHub: "ubuntu-24.04", GitHubMinutes: 2000,
			Vars: []string{"CI_RUNNER"}, BotVars: []string{"BOT_RUNNER"}},
		"hosted only": {Owner: "octo", Repos: []string{"octo/a", "octo/b"}, GitHub: "ubuntu-latest", GitHubMinutes: 3000, Vars: []string{"CI_RUNNER"}},
	}
	for name, d := range drafts {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "zerobill.yaml")
			if err := os.WriteFile(path, []byte(d.Render(now)), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := config.Load(path)
			if err != nil {
				t.Fatalf("%v\n%s", err, d.Render(now))
			}
			providers, err := c.PolicyProviders()
			if err != nil {
				t.Fatal(err)
			}
			routes := c.PolicyRoutes(providers)
			if c.Mode != config.DryRun || len(c.Repositories) != len(d.Repos) || len(routes) != 1+len(d.BotVars) {
				t.Fatalf("unexpected config %+v routes %+v", c, routes)
			}
			if d.SelfHosted != "" && (routes[0].Providers[0].Label != "home-ci" || !routes[0].Providers[0].OnDemand || routes[0].Balance != "pace") {
				t.Fatalf("ci route %+v", routes[0])
			}
			if d.SelfHosted == "" && (c.Fallback != "github" || providers[0].MonthlyMinutes != 3000) {
				t.Fatalf("hosted-only config %+v", c)
			}
		})
	}
}

func TestTokenURL(t *testing.T) {
	u, err := url.Parse(setup.TokenURL("acme", true, false))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("target_name") != "acme" || q.Get("actions") != "read" || q.Get("actions_variables") != "read" ||
		q.Get("organization_self_hosted_runners") != "read" || q.Get("organization_administration") != "read" || q.Get("plan") != "" {
		t.Fatalf("organization dry-run token %s", u)
	}
	q, _ = url.ParseQuery(strings.SplitN(setup.TokenURL("octo", false, true), "?", 2)[1])
	if q.Get("actions") != "write" || q.Get("actions_variables") != "write" || q.Get("plan") != "read" || q.Get("administration") != "read" {
		t.Fatalf("user switch token %v", q)
	}
}

func TestComposeMatchesExample(t *testing.T) {
	raw, err := os.ReadFile("../../examples/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != setup.Compose {
		t.Fatal("internal/setup/compose.yaml differs from examples/compose.yaml; copy the example over it")
	}
}
