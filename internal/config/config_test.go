package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	c, err := Load(write("owner: acme\nself_hosted_label: home-ci\nblacksmith: true\n"))
	if err != nil || *c != (Config{Owner: "acme", SelfHostedLabel: "home-ci", Blacksmith: true, ReservePercent: DefaultReservePercent, AlertRemainingPercent: DefaultAlertRemainingPercent, CheckInterval: DefaultCheckInterval, QueueTimeout: DefaultQueueTimeout}) {
		t.Fatalf("got %+v %v", c, err)
	}
	for body, want := range map[string]string{
		"self_hosted_label: home-ci\n":     "owner is required",
		"owner: acme\nmode: switch\n":      "supported settings",
		"owner: acme\nnotify: ntfy.sh/x\n": "notify must be an http or https URL",
		"owner: [\n":                       "config.yaml",
	} {
		if _, err := Load(write(body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", body, err, want)
		}
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil || !strings.Contains(err.Error(), "zerobill init") {
		t.Errorf("missing file: %v", err)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("ZEROBILL_RESERVE_PERCENT", "")
	t.Setenv("ZEROBILL_OWNER", "")
	if _, ok, err := FromEnv(); ok || err != nil {
		t.Fatalf("unset owner: %v %v", ok, err)
	}
	t.Setenv("ZEROBILL_OWNER", "acme")
	t.Setenv("ZEROBILL_SELF_HOSTED_LABEL", "home-ci")
	t.Setenv("ZEROBILL_BLACKSMITH", "true")
	t.Setenv("ZEROBILL_NOTIFY", "https://ntfy.sh/x")
	c, ok, err := FromEnv()
	if !ok || err != nil || *c != (Config{Owner: "acme", SelfHostedLabel: "home-ci", Blacksmith: true, Notify: "https://ntfy.sh/x", ReservePercent: DefaultReservePercent, AlertRemainingPercent: DefaultAlertRemainingPercent, CheckInterval: DefaultCheckInterval, QueueTimeout: DefaultQueueTimeout}) {
		t.Fatalf("got %+v %v %v", c, ok, err)
	}
	t.Setenv("ZEROBILL_BLACKSMITH", "yes")
	if _, _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "use true or false") {
		t.Fatalf("bad bool: %v", err)
	}
}

func TestReservePercent(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  int
		valid bool
	}{
		{"", 10, true}, {"0", 0, true}, {"25", 25, true}, {"99", 99, true},
		{"-1", 0, false}, {"100", 0, false}, {"1.5", 0, false}, {"1.0", 0, false}, {".nan", 0, false}, {"ten", 0, false},
	} {
		t.Run("value="+tt.value, func(t *testing.T) {
			t.Setenv("ZEROBILL_OWNER", "acme")
			t.Setenv("ZEROBILL_BLACKSMITH", "")
			t.Setenv("ZEROBILL_NOTIFY", "")
			t.Setenv("ZEROBILL_RESERVE_PERCENT", tt.value)
			c, _, err := FromEnv()
			if (err == nil) != tt.valid || (err == nil && c.ReservePercent != tt.want) {
				t.Fatalf("env: %+v %v", c, err)
			}
			body := "owner: acme\n"
			if tt.value != "" {
				body += "reserve_percent: " + tt.value + "\n"
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err = Load(path)
			if (err == nil) != tt.valid || (err == nil && c.ReservePercent != tt.want) {
				t.Fatalf("file: %+v %v", c, err)
			}
		})
	}
}

func TestRejectNullReserve(t *testing.T) {
	for _, value := range []string{"null", "~", ""} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("owner: acme\nreserve_percent: "+value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "reserve_percent must be an integer") {
			t.Errorf("%q: want integer error, got %v", value, err)
		}
	}
}

func TestOptionalSettings(t *testing.T) {
	for _, tt := range []struct {
		key, env, value string
		valid           bool
	}{
		{"alert_remaining_percent", "ZEROBILL_ALERT_REMAINING_PERCENT", "5", true},
		{"alert_remaining_percent", "ZEROBILL_ALERT_REMAINING_PERCENT", "0", true},
		{"alert_remaining_percent", "ZEROBILL_ALERT_REMAINING_PERCENT", "100", false},
		{"alert_remaining_percent", "ZEROBILL_ALERT_REMAINING_PERCENT", "-1", false},
		{"alert_remaining_percent", "ZEROBILL_ALERT_REMAINING_PERCENT", "1.5", false},
		{"alert_remaining_percent", "ZEROBILL_ALERT_REMAINING_PERCENT", "null", false},
		{"check_interval", "ZEROBILL_CHECK_INTERVAL", "1m", true},
		{"check_interval", "ZEROBILL_CHECK_INTERVAL", "1h", true},
		{"check_interval", "ZEROBILL_CHECK_INTERVAL", "59s", false},
		{"check_interval", "ZEROBILL_CHECK_INTERVAL", "0", false},
		{"queue_timeout", "ZEROBILL_QUEUE_TIMEOUT", "2m", true},
		{"queue_timeout", "ZEROBILL_QUEUE_TIMEOUT", "61m", false},
		{"queue_timeout", "ZEROBILL_QUEUE_TIMEOUT", "soon", false},
	} {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("owner: acme\n"+tt.key+": "+tt.value+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := Load(path)
			if (err == nil) != tt.valid {
				t.Fatalf("file: %+v %v", file, err)
			}
			t.Setenv("ZEROBILL_OWNER", "acme")
			t.Setenv(tt.env, tt.value)
			env, _, err := FromEnv()
			if (err == nil) != tt.valid {
				t.Fatalf("env: %+v %v", env, err)
			}
			if tt.valid && *file != *env {
				t.Fatalf("different config: file %+v env %+v", file, env)
			}
		})
	}
}

func TestLegacyRunnerExplainsMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("owner: acme\nrunner: home-ci\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "rename runner to self_hosted_label") {
		t.Fatalf("file: %v", err)
	}
	t.Setenv("ZEROBILL_OWNER", "acme")
	t.Setenv("ZEROBILL_RUNNER", "home-ci")
	if _, _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "rename ZEROBILL_RUNNER") {
		t.Fatalf("env: %v", err)
	}
}

func TestCommentedConfigRoundTrip(t *testing.T) {
	for _, full := range []bool{false, true} {
		c := Defaults()
		c.Owner = "acme"
		if full {
			c.Owner = "owner: quoted"
			c.SelfHostedLabel, c.Blacksmith = "home-ci", true
			c.CheckInterval, c.QueueTimeout = time.Minute, 2*time.Minute
			c.ReservePercent, c.AlertRemainingPercent = 0, 5
			c.Notify = "https://ntfy.sh/topic#fragment"
		}
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := Create(path, c); err != nil {
			t.Fatal(err)
		}
		got, err := Load(path)
		if err != nil || *got != c {
			t.Fatalf("round trip: %+v %v want %+v", got, err, c)
		}
		raw, _ := os.ReadFile(path)
		for _, field := range []string{"self_hosted_label", "blacksmith", "reserve_percent", "alert_remaining_percent", "check_interval", "queue_timeout", "notify"} {
			if !full && !strings.Contains(string(raw), "# "+field+":") {
				t.Fatalf("missing commented option %s", field)
			}
		}
	}
}
