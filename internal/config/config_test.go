package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPaths(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	t.Setenv("XDG_STATE_HOME", "/state")
	if p, _ := Path(); p != "/cfg/zerobill/config.yaml" {
		t.Fatalf("config path %s", p)
	}
	if p, _ := StateDir(); p != "/state/zerobill" {
		t.Fatalf("state dir %s", p)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/ada")
	if p, _ := Path(); p != "/home/ada/.config/zerobill/config.yaml" {
		t.Fatalf("default config path %s", p)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c, err := Load(write("owner: acme\nrunner: home-ci\nblacksmith: true\n"))
	if err != nil || *c != (Config{Owner: "acme", Runner: "home-ci", Blacksmith: true}) {
		t.Fatalf("got %+v %v", c, err)
	}
	for body, want := range map[string]string{
		"runner: home-ci\n":                "owner is required",
		"owner: acme\nmode: switch\n":      "takes only owner, runner, blacksmith and notify",
		"owner: acme\nnotify: ntfy.sh/x\n": "notify must be an http or https URL",
		"owner: [\n":                       "config.yaml",
	} {
		if _, err := Load(write(body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", body, err, want)
		}
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil || !strings.Contains(err.Error(), "owner: <your user or organization>") {
		t.Errorf("missing file: %v", err)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("ZEROBILL_OWNER", "")
	if _, ok, err := FromEnv(); ok || err != nil {
		t.Fatalf("unset owner: %v %v", ok, err)
	}
	t.Setenv("ZEROBILL_OWNER", "acme")
	t.Setenv("ZEROBILL_RUNNER", "home-ci")
	t.Setenv("ZEROBILL_BLACKSMITH", "true")
	t.Setenv("ZEROBILL_NOTIFY", "https://ntfy.sh/x")
	c, ok, err := FromEnv()
	if !ok || err != nil || *c != (Config{Owner: "acme", Runner: "home-ci", Blacksmith: true, Notify: "https://ntfy.sh/x"}) {
		t.Fatalf("got %+v %v %v", c, ok, err)
	}
	t.Setenv("ZEROBILL_BLACKSMITH", "yes")
	if _, _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "use true or false") {
		t.Fatalf("bad bool: %v", err)
	}
}
