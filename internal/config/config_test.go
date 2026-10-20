package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ICARO_HOME", home)
	t.Setenv("ICARO_CONFIG", "")
	return home
}

func TestLoadDefaultsAndEnvOverride(t *testing.T) {
	home := isolate(t)
	t.Setenv("ICARO_DATABASE__DRIVER", "postgres")
	t.Setenv("ICARO_DATABASE__DSN", "postgres://x")
	t.Setenv("ICARO_WORKFLOWS__CONCURRENCY", "7")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Home != home {
		t.Fatalf("home = %s", cfg.Home)
	}
	if cfg.Database.Driver != "postgres" || cfg.Database.DSN != "postgres://x" {
		t.Fatalf("env override not applied: %+v", cfg.Database)
	}
	if cfg.Workflows.Concurrency != 7 || cfg.Limits.Timeout != 30*time.Minute {
		t.Fatalf("workflows=%+v limits=%+v", cfg.Workflows, cfg.Limits)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Profiles["claude"].Egress == nil || len(cfg.Profiles["claude"].Egress) == 0 {
		t.Fatal("claude profile must ship an egress allowlist")
	}
}

func TestLoadFileFromHome(t *testing.T) {
	home := isolate(t)
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "version: 1\ndaemon:\n  listen: 127.0.0.1:1\nprofiles:\n  claude:\n    egress: [api.anthropic.com]\n  web:\n    egress: ['*.npmjs.org']\n"
	if err := os.WriteFile(filepath.Join(home, "config", "icaro.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.Listen != "127.0.0.1:1" {
		t.Fatalf("file not applied: %+v", cfg.Daemon)
	}
	if got := cfg.Profiles["claude"]; got.Agent != "claude" || len(got.Egress) != 1 || got.Image == "" {
		t.Fatalf("profile merge: %+v", got)
	}
	if cfg.Profiles["web"].Image != DefaultAgentImage {
		t.Fatal("new profile gets the default image")
	}
	if cfg.SQLitePath() != filepath.Join(home, "config", "icaro.db") {
		t.Fatalf("sqlite path = %s", cfg.SQLitePath())
	}
	if ResolvedPath("") == "" {
		t.Fatal("resolved path")
	}
}

func TestValidate(t *testing.T) {
	isolate(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Version = 99
	cfg.Profiles["bad name"] = ProfileConfig{}
	cfg.Profiles["noegress"] = ProfileConfig{}
	cfg.Profiles["url"] = ProfileConfig{Egress: []string{"https://x"}}
	cfg.Sessions.DefaultProfile = "nope"
	cfg.Database.Driver = "mysql"
	err = cfg.Validate()
	for _, want := range []string{"version 99", "bad name", "noegress.egress is mandatory", "hostname", "default_profile", "mysql"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
}

func TestExplicitMissingFile(t *testing.T) {
	isolate(t)
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("missing explicit config must fail")
	}
	if envKey("ICARO_MASTER_KEY") != "" || envKey("ICARO_DAEMON__LISTEN") != "daemon.listen" {
		t.Fatal("envKey")
	}
}
