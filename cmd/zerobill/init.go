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
		return fmt.Errorf("config already exists at %s; edit it to change settings", path)
	} else if !errors.Is(err, os.ErrNotExist) {
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
	c := config.Config{ReservePercent: config.DefaultReservePercent}
	for c.Owner == "" {
		c.Owner, err = ask("GitHub owner or organization: ")
		if err != nil {
			return err
		}
	}
	c.Runner, err = ask("Self-hosted runner label (optional): ")
	if err != nil {
		return err
	}
	for {
		answer, err := ask("Use Blacksmith's free tier? [y/N]: ")
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
	_, err = fmt.Fprintf(out, "\nCreated %s\nNext: zerobill status\n", path)
	return err
}
