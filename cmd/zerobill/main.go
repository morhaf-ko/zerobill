// Command zerobill decides which runner label keeps CI inside free capacity.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/morhaf-ko/zerobill/internal/config"
	"github.com/morhaf-ko/zerobill/internal/policy"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "plan" {
		fmt.Fprintln(os.Stderr, "usage: zerobill plan --config zerobill.yaml --observed observed.yaml")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	cfgPath := fs.String("config", "zerobill.yaml", "routing policy")
	obsPath := fs.String("observed", "", "provider state snapshot")
	_ = fs.Parse(os.Args[2:])
	if *obsPath == "" {
		fail("--observed is required until live collectors land")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fail(err.Error())
	}
	providers, err := cfg.PolicyProviders()
	if err != nil {
		fail(err.Error())
	}
	states, err := config.LoadObservation(*obsPath)
	if err != nil {
		fail(err.Error())
	}
	d, err := policy.Decide(providers, states, cfg.Fallback)
	if err != nil {
		fail(err.Error())
	}

	fmt.Printf("mode: %s\n", cfg.Mode)
	fmt.Printf("decision: %s -> %s\n", d.Provider, d.Label)
	fmt.Printf("reason: %s\n", d.Reason)
	for _, repo := range cfg.Repositories {
		fmt.Printf("would set %s on %s to %s\n", strings.Join(cfg.Variables, ", "), repo, d.Label)
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "zerobill:", msg)
	os.Exit(1)
}
