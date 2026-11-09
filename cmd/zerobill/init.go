package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/morhaf-ko/zerobill/internal/config"
)

func initConfig(args []string, in io.Reader, out io.Writer) error {
	if len(args) != 0 {
		return errors.New("init takes no arguments")
	}
	path, err := config.Path()
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("config already exists at %s; edit it to change settings.\nNext: zerobill status", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if _, err := fmt.Fprintln(out, "Create a local config. Status will check your GitHub access afterwards.\nGitHub provides runner, workflow and usage data. Blacksmith usage is estimated\nfrom GitHub Actions jobs; no Blacksmith API key or CLI is needed."); err != nil {
		return err
	}
	scan := bufio.NewScanner(in)
	ask := func(prompt string) (string, error) {
		if _, err := fmt.Fprint(out, prompt); err != nil {
			return "", err
		}
		if !scan.Scan() {
			if err := scan.Err(); err != nil {
				return "", err
			}
			return "", errors.New("input ended; config not created")
		}
		return strings.TrimSpace(scan.Text()), nil
	}
	c := config.Defaults()
	for c.Owner == "" {
		c.Owner, err = ask("GitHub owner or organization: ")
		if err != nil {
			return err
		}
	}
	c.SelfHostedLabel, err = ask("Optional self-hosted runners: use the label shown in GitHub > Settings > Actions > Runners.\nOnly include machines you want used automatically, such as your server.\nSelf-hosted label (leave blank for GitHub/Blacksmith only): ")
	if err != nil {
		return err
	}
	for {
		answer, err := ask("Use Blacksmith's free tier (requires its GitHub App)? [y/N]: ")
		if err != nil {
			return err
		}
		switch strings.ToLower(answer) {
		case "y", "yes":
			c.Blacksmith = true
		case "", "n", "no":
		default:
			continue
		}
		break
	}
	if err := config.Create(path, c); err != nil {
		return fmt.Errorf("create config: %w", err)
	}
	fmt.Fprintf(out, "\nCreated %s\nOptional defaults are commented out; uncomment a setting to change it.\n", path)
	if source := credentialSource(); source != "GitHub CLI" {
		fmt.Fprintf(out, "GitHub credential: %s (overrides the stored GitHub CLI login).\n", source)
	} else {
		fmt.Fprintln(out, "GitHub credential: your GitHub CLI login. Sign in if needed:\n  gh auth login --hostname github.com")
	}
	if c.Blacksmith {
		fmt.Fprintln(out, "Make sure the Blacksmith GitHub App is installed for this owner: https://www.blacksmith.sh")
	}
	_, err = fmt.Fprintln(out, "Next: zerobill status\nStatus checks access and tells you which setup step is needed next.")
	return err
}
