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
	if len(cfg.Profiles["claude"].Egress) == 0 {
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

func TestValidateRanges(t *testing.T) {
	isolate(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Version = 0
	cfg.Home = ""
	cfg.Database.Driver = "postgres"
	cfg.Database.DSN = ""
	cfg.Daemon.UIListen = "0.0.0.0:8787"
	cfg.Workflows.Concurrency = 0
	cfg.Limits.Timeout = 0
	cfg.Sessions.MaxActive = -1
	cfg.Workflows.Profile = "ghost"
	err = cfg.Validate()
	for _, want := range []string{"version 0", "home is required", "database.dsn is required", "daemon.ui_listen", "workflows.concurrency", "limits.timeout", "sessions.max_active", `workflows.profile "ghost"`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
}

// TestValidateRemoteListen is the regression test for daemon.listen
// accepting any address with plain HTTP (MVP cut 2).
func TestValidateRemoteListen(t *testing.T) {
	isolate(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"0.0.0.0:8787", ":8787", "10.0.0.1:80"} {
		cfg.Daemon.Listen, cfg.Daemon.AllowRemote = addr, false
		if !cfg.Daemon.RemoteListen() {
			t.Errorf("%s: not remote", addr)
		}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "daemon.allow_remote") {
			t.Errorf("%s accepted without allow_remote: %v", addr, err)
		}
		cfg.Daemon.AllowRemote = true
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s with allow_remote: %v", addr, err)
		}
	}
	for _, addr := range []string{"", "127.0.0.1:8787", "localhost:0"} {
		cfg.Daemon.Listen, cfg.Daemon.AllowRemote = addr, false
		if cfg.Daemon.RemoteListen() || cfg.Validate() != nil {
			t.Errorf("%s: refused: %v", addr, cfg.Validate())
		}
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8787": true,
		"[::1]:8787":     true,
		"localhost:1":    true,
		"0.0.0.0:8787":   false,
		"10.0.0.1:80":    false,
		"example.com:80": false,
		"127.0.0.1":      false, // no port
	} {
		if got := isLoopback(addr); got != want {
			t.Errorf("%s: %v", addr, got)
		}
	}
}

func TestPathsAndConfigFile(t *testing.T) {
	home := isolate(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogsDir() != cfg.Layout().Logs() || !strings.HasPrefix(cfg.LogsDir(), home) {
		t.Fatalf("logs dir: %s", cfg.LogsDir())
	}
	cfg.Database.DSN = "/srv/icaro.db"
	if cfg.SQLitePath() != "/srv/icaro.db" {
		t.Fatal("an explicit sqlite DSN wins")
	}
	if ResolvedPath("") != "" {
		t.Fatal("no config file yet")
	}

	// ICARO_CONFIG names the file; a missing one is an error.
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("workflows:\n  concurrency: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICARO_CONFIG", p)
	cfg, err = Load("")
	if err != nil || cfg.Workflows.Concurrency != 3 {
		t.Fatalf("ICARO_CONFIG: %v %+v", err, cfg)
	}
	t.Setenv("ICARO_CONFIG", p+".missing")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "ICARO_CONFIG") {
		t.Fatalf("missing ICARO_CONFIG: %v", err)
	}

	// A malformed file names itself in the error.
	if err := os.WriteFile(p, []byte("workflows: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("bad yaml: %v", err)
	}
	// A value of the wrong type fails to unmarshal.
	if err := os.WriteFile(p, []byte("workflows:\n  concurrency: lots\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("non-numeric concurrency must fail")
	}
}

func TestExpandHome(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	if got := expandHome("~/x"); got != "/home/u/x" {
		t.Fatalf("expand: %s", got)
	}
	if got := expandHome("/abs"); got != "/abs" {
		t.Fatalf("abs: %s", got)
	}
}
