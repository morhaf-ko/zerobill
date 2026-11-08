package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/morhaf-ko/zerobill/internal/config"
)

func TestInit(t *testing.T) {
	for _, tt := range []struct {
		name, input, runner string
		blacksmith          bool
	}{
		{"minimal", "acme\n\n\n", "", false},
		{"full", "acme\nhome-ci\nyes\n", "home-ci", true},
		{"retry answers", " \n acme \n home-ci \nmaybe\nY\n", "home-ci", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			var out bytes.Buffer
			if err := initConfig(nil, strings.NewReader(tt.input), &out); err != nil {
				t.Fatal(err)
			}
			path, _ := config.Path()
			c, err := config.Load(path)
			if err != nil || c.Owner != "acme" || c.Runner != tt.runner || c.Blacksmith != tt.blacksmith || c.ReservePercent != 10 || c.Notify != "" {
				t.Fatalf("config %+v: %v", c, err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("config permissions: %v %v", info, err)
			}
			info, err = os.Stat(filepath.Dir(path))
			if err != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("directory permissions: %v %v", info, err)
			}
			if !strings.Contains(out.String(), "Next: zerobill status") {
				t.Fatalf("output %q", out.String())
			}
		})
	}
}

func TestInitLeavesNoConfigOnIncompleteInput(t *testing.T) {
	for _, input := range []string{"", "acme\n", "acme\n\n", "acme\n\nmaybe\n", strings.Repeat("x", 70_000)} {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		if err := initConfig(nil, strings.NewReader(input), &bytes.Buffer{}); err == nil {
			t.Fatal("want input error")
		}
		path, _ := config.Path()
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("created config on incomplete input: %v", err)
		}
	}
}

func TestInitPreservesExistingConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, _ := config.Path()
	if err := config.Create(path, config.Config{Owner: "original", ReservePercent: 0}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := initConfig(nil, strings.NewReader("acme\n\ny\n"), &out); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing config: %v", err)
	}
	c, err := config.Load(path)
	if err != nil || c.Owner != "original" || c.ReservePercent != 0 || out.Len() != 0 {
		t.Fatalf("existing config changed: %+v %v, output %q", c, err, out.String())
	}
}

func TestInitRejectsArguments(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := initConfig([]string{"--force"}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatal("want argument error")
	}
}
