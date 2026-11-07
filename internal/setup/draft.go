package setup

import (
	_ "embed"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Compose is examples/compose.yaml, which init writes next to a new config.
//
//go:embed compose.yaml
var Compose string

// Draft is what init detected about an owner.
type Draft struct {
	Owner string
	Repos []string
	// SelfHosted is the self-hosted runner label workflows use, empty if none.
	SelfHosted string
	// OnDemand is set when runners for SelfHosted are created per job: no runner
	// carries the label now, or a GARM pool registered the ones that do.
	OnDemand   bool
	Blacksmith string
	GitHub     string
	// GitHubMinutes is the plan's included Linux minutes.
	GitHubMinutes float64
	// Vars are the variables workflows read in runs-on; BotVars the subset for
	// dependency bots.
	Vars    []string
	BotVars []string
}

// Render writes the draft as a commented zerobill.yaml in dry-run mode.
func (d Draft) Render(now time.Time) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("# Written by zerobill init for %s on %s. Review it, then run\n", d.Owner, now.Format("2006-01-02"))
	w("#   zerobill doctor --config zerobill.yaml\n")
	w("# dry-run logs decisions only; set mode: switch once they look right.\n")
	w("mode: dry-run\nmin_hold: 15m\nowner: %s\n", d.Owner)
	w("# Private repositories with workflows. In switch mode zerobill sets the route\n# variables in each.\nrepositories:\n")
	for _, r := range d.Repos {
		w("  - %s\n", r)
	}

	var ci, metered []string
	fallback := "github"
	w("providers:\n")
	if d.SelfHosted != "" {
		fallback = "home"
		ci = append(ci, "home")
		w("  - name: home\n    label: %s\n    kind: self-hosted\n", d.SelfHosted)
		w("    # A job waiting this long means nothing is picking up the label.\n    max_queue_wait: 10m\n")
		if d.OnDemand {
			w("    # Runners for this label are created per job, so an empty runner list is\n    # normal and only a queue stall marks the provider down.\n    on_demand: true\n")
		} else {
			w("    # Set on_demand: true if runners are created per job (GARM, ARC).\n")
		}
	} else {
		w("  # No self-hosted runner found. Add one here to run CI on your own machine;\n  # see docs/homelab.md.\n")
	}
	if d.Blacksmith != "" {
		metered = append(metered, "blacksmith")
		w("  - name: blacksmith\n    label: %s\n    kind: metered\n", d.Blacksmith)
		w("    # Blacksmith bills past its free minutes; the reserve stops zerobill early.\n")
		w("    monthly_minutes: 3000\n    reserve_minutes: 300\n")
	}
	metered = append(metered, "github")
	w("  - name: github\n    label: %s\n    kind: metered\n", d.GitHub)
	w("    monthly_minutes: %.0f\n    reserve_minutes: %.0f\n", d.GitHubMinutes, d.GitHubMinutes/10)
	ci = append(ci, metered...)
	if d.SelfHosted == "" {
		w("# With no self-hosted runner, jobs fall back to GitHub-hosted. GitHub Free stops\n# hosted jobs at the included usage unless a budget allows more.\n")
	}
	w("fallback: %s\n", fallback)

	balance := "order"
	if len(metered) > 1 {
		balance = "pace"
	}
	w("routes:\n  - name: ci\n    variables: [%s]\n    providers: [%s]\n    balance: %s\n", strings.Join(d.Vars, ", "), strings.Join(ci, ", "), balance)
	if len(d.BotVars) > 0 {
		w("  # Dependency bots use hosted free tiers only, so your machine stays free.\n")
		w("  - name: bot\n    variables: [%s]\n    providers: [%s]\n    balance: %s\n", strings.Join(d.BotVars, ", "), strings.Join(metered, ", "), balance)
	} else {
		w("  # To keep dependency bots off your machine, add a bot route; see the README.\n")
	}
	return b.String()
}

// TokenURL returns a GitHub link that pre-fills a fine-grained token with the
// permissions zerobill needs for owner. Write access is only needed for switch mode.
func TokenURL(owner string, org, write bool) string {
	access := "read"
	if write {
		access = "write"
	}
	q := url.Values{
		"name":              {"zerobill"},
		"description":       {"zerobill: read runners, runs and billing; set runner variables"},
		"target_name":       {owner},
		"expires_in":        {"90"},
		"actions":           {access},
		"actions_variables": {access},
		"contents":          {"read"},
		"metadata":          {"read"},
	}
	if org {
		q.Set("organization_self_hosted_runners", "read")
		q.Set("organization_administration", "read")
	} else {
		q.Set("administration", "read")
		q.Set("plan", "read")
	}
	return "https://github.com/settings/personal-access-tokens/new?" + q.Encode()
}

// MostCommon returns the most frequent string matching keep, ties broken by
// order, or "" when none matches.
func MostCommon(items []string, keep func(string) bool) string {
	counts := map[string]int{}
	for _, it := range items {
		if keep(it) {
			counts[it]++
		}
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

// Hosted reports whether a label names a GitHub-hosted or Blacksmith runner.
func Hosted(label string) bool {
	for _, p := range []string{"ubuntu-", "windows-", "macos-", "blacksmith-"} {
		if strings.HasPrefix(label, p) {
			return true
		}
	}
	return false
}
