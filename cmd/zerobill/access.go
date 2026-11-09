package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/morhaf-ko/zerobill/internal/github"
)

func credentialSource() string {
	if os.Getenv("GITHUB_TOKEN") != "" {
		return "GITHUB_TOKEN"
	}
	if os.Getenv("GH_TOKEN") != "" {
		return "GH_TOKEN"
	}
	return "GitHub CLI"
}

func accessSteps(source string, org bool) string {
	if source == "GITHUB_TOKEN" || source == "GH_TOKEN" {
		return source + " overrides your stored GitHub CLI login. To try that login for one command:\n" +
			"  env -u GITHUB_TOKEN -u GH_TOKEN zerobill status\n" +
			"To keep using a token, check its owner, repository selection and permissions:\n" +
			"  https://github.com/morhaf-ko/zerobill/blob/main/SECURITY.md"
	}
	scopes := "repo,user"
	if org {
		scopes = "repo,admin:org"
	}
	help := "Authorize the GitHub CLI to read private repositories, runners and billing:\n" +
		"  gh auth refresh --hostname github.com --scopes " + scopes + "\n" +
		"Then check again:\n  zerobill status"
	if org {
		help += "\nOrganization billing and runner access also require the appropriate organization role."
	}
	return help
}

func connectionProblem(action, source, owner string, org bool, err error) error {
	if github.IsRateLimited(err) {
		return fmt.Errorf("GitHub is rate limiting requests; wait until its rate limit resets before running zerobill status again.\nDetails: %w", err)
	}
	if github.IsAccessDenied(err) {
		if se, ok := err.(*github.StatusError); ok && se.Code == 401 && source == "GitHub CLI" {
			return fmt.Errorf("your GitHub CLI login was rejected. Sign in again:\n  gh auth login --hostname github.com\n  zerobill status\n\nDetails: %w", err)
		}
		return fmt.Errorf("cannot %s using %s.\n\nNext: %s\n\nDetails: %w", action, source, accessSteps(source, org), err)
	}
	if github.IsNotFound(err) {
		return fmt.Errorf("cannot find owner %s, or this credential cannot see it. Check owner in your config and the account shown by:\n  gh auth status --hostname github.com\n\nDetails: %w", owner, err)
	}
	return fmt.Errorf("could not %s using %s; check the connection and try zerobill status again.\nDetails: %w", action, source, err)
}

func billingURL(owner string, org bool) string {
	if org {
		return "https://github.com/organizations/" + url.PathEscape(owner) + "/settings/billing/usage"
	}
	return "https://github.com/settings/billing/usage"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
