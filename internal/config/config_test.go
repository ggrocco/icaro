package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaultsAndEnvOverride(t *testing.T) {
	t.Setenv("ICARO_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ICARO_DATABASE__DRIVER", "postgres")
	t.Setenv("ICARO_DATABASE__DSN", "postgres://x")
	t.Setenv("ICARO_RUNNER__CONCURRENCY", "4")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.Driver != "postgres" || cfg.Database.DSN != "postgres://x" {
		t.Fatalf("env override not applied: %+v", cfg.Database)
	}
	if cfg.Runner.Concurrency != 4 {
		t.Fatalf("concurrency = %d", cfg.Runner.Concurrency)
	}
	if cfg.Runner.Defaults.Timeout != 30*time.Minute {
		t.Fatalf("timeout default = %s", cfg.Runner.Defaults.Timeout)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "icaro.yaml")
	if err := os.WriteFile(path, []byte("data_dir: "+dir+"\nserver:\n  listen: \":1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != dir || cfg.Server.Listen != ":1" {
		t.Fatalf("file not applied: %+v", cfg)
	}
	if cfg.SQLitePath() != filepath.Join(dir, "icaro.db") {
		t.Fatalf("sqlite path = %s", cfg.SQLitePath())
	}
}

func TestValidateErrors(t *testing.T) {
	cfg := &Config{DataDir: "x", Database: DatabaseConfig{Driver: "mysql"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error")
	}
}
