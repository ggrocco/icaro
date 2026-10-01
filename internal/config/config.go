// Package config loads icaro configuration from defaults, an optional YAML
// file and ICARO_* environment variables (nested keys joined with "__").
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

const envPrefix = "ICARO_"

// Config is the full icaro configuration.
type Config struct {
	DataDir  string         `koanf:"data_dir"`
	Server   ServerConfig   `koanf:"server"`
	Database DatabaseConfig `koanf:"database"`
	Runner   RunnerConfig   `koanf:"runner"`
	HTTPStep HTTPStepConfig `koanf:"http_step"`
}

// ServerConfig holds listen settings for `icaro serve` and the client
// settings (URL + token) used by CLI commands that talk to a server.
type ServerConfig struct {
	Listen string `koanf:"listen"`
	URL    string `koanf:"url"`
	Token  string `koanf:"token"`
}

// DatabaseConfig selects the storage backend.
type DatabaseConfig struct {
	Driver string `koanf:"driver"` // sqlite | postgres
	DSN    string `koanf:"dsn"`    // empty for sqlite => <data_dir>/icaro.db
}

// RunnerConfig controls step execution.
type RunnerConfig struct {
	Concurrency   int           `koanf:"concurrency"`
	PollInterval  time.Duration `koanf:"poll_interval"`
	DockerHost    string        `koanf:"docker_host"`    // empty => docker SDK defaults
	StrictRuntime string        `koanf:"strict_runtime"` // e.g. "runsc"; empty => default runtime
	Defaults      StepDefaults  `koanf:"defaults"`
}

// StepDefaults are the resource ceilings applied to every step container.
// Workflows may only tighten them.
type StepDefaults struct {
	Timeout   time.Duration `koanf:"timeout"`
	MemoryMB  int64         `koanf:"memory_mb"`
	CPUs      float64       `koanf:"cpus"`
	PidsLimit int64         `koanf:"pids_limit"`
	NoFile    int64         `koanf:"nofile"`
}

// HTTPStepConfig controls the in-process http step.
type HTTPStepConfig struct {
	AllowPrivateNetworks bool `koanf:"allow_private_networks"`
}

// Defaults returns the baseline configuration.
func Defaults() map[string]any {
	return map[string]any{
		"data_dir":                         defaultDataDir(),
		"server.listen":                    "127.0.0.1:8787",
		"server.url":                       "http://127.0.0.1:8787",
		"database.driver":                  "sqlite",
		"runner.concurrency":               2,
		"runner.poll_interval":             "1s",
		"runner.defaults.timeout":          "30m",
		"runner.defaults.memory_mb":        1024,
		"runner.defaults.cpus":             1.0,
		"runner.defaults.pids_limit":       256,
		"runner.defaults.nofile":           4096,
		"http_step.allow_private_networks": false,
	}
}

// Load builds a Config from defaults, the config file (explicit path, or the
// first of $ICARO_CONFIG, ./icaro.yaml, $XDG_CONFIG_HOME/icaro/icaro.yaml that
// exists) and the environment.
func Load(path string) (*Config, error) {
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(Defaults(), "."), nil); err != nil {
		return nil, err
	}

	resolved, err := resolveConfigPath(path)
	if err != nil {
		return nil, err
	}
	if resolved != "" {
		if err := k.Load(file.Provider(resolved), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("config %s: %w", resolved, err)
		}
	}

	if err := k.Load(env.Provider(envPrefix, ".", func(s string) string {
		s = strings.TrimPrefix(s, envPrefix)
		return strings.ReplaceAll(strings.ToLower(s), "__", ".")
	}), nil); err != nil {
		return nil, err
	}

	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return nil, err
	}
	cfg.DataDir = expandHome(cfg.DataDir)
	return &cfg, nil
}

// Validate checks invariants that cannot be expressed structurally.
func (c *Config) Validate() error {
	var errs []error
	if c.DataDir == "" {
		errs = append(errs, errors.New("data_dir is required"))
	}
	switch c.Database.Driver {
	case "sqlite", "postgres":
	default:
		errs = append(errs, fmt.Errorf("database.driver must be sqlite or postgres, got %q", c.Database.Driver))
	}
	if c.Database.Driver == "postgres" && c.Database.DSN == "" {
		errs = append(errs, errors.New("database.dsn is required for postgres"))
	}
	if c.Runner.Concurrency < 1 {
		errs = append(errs, errors.New("runner.concurrency must be >= 1"))
	}
	if c.Runner.Defaults.Timeout <= 0 {
		errs = append(errs, errors.New("runner.defaults.timeout must be > 0"))
	}
	return errors.Join(errs...)
}

// SQLitePath is the database file used when the driver is sqlite and no DSN
// is configured.
func (c *Config) SQLitePath() string {
	if c.Database.DSN != "" {
		return c.Database.DSN
	}
	return filepath.Join(c.DataDir, "icaro.db")
}

// LogsDir is where step logs are written.
func (c *Config) LogsDir() string { return filepath.Join(c.DataDir, "logs") }

func resolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("config file: %w", err)
		}
		return explicit, nil
	}
	candidates := []string{os.Getenv("ICARO_CONFIG"), "icaro.yaml"}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "icaro", "icaro.yaml"))
	} else if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "icaro", "icaro.yaml"))
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", nil
}

func defaultDataDir() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "icaro")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "./data"
	}
	return filepath.Join(home, ".local", "share", "icaro")
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
