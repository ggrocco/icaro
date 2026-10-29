// Package config loads Ícaro configuration from defaults, an optional YAML
// file and ICARO_* environment variables (nested keys joined with "__").
//
// The file is versioned: `version` names the config schema, and a file
// written for a newer schema is rejected instead of half-applied.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"icaro/internal/paths"
)

const envPrefix = "ICARO_"

// CurrentVersion is the config schema version this binary understands.
const CurrentVersion = 1

// DefaultAgentImage is the agent base image (architecture §5).
const DefaultAgentImage = "ghcr.io/icaro-dev/agent-base:latest"

// Config is the full configuration.
type Config struct {
	Version   int                      `yaml:"version"`
	Home      string                   `yaml:"home"`
	Daemon    DaemonConfig             `yaml:"daemon"`
	Database  DatabaseConfig           `yaml:"database"`
	Docker    DockerConfig             `yaml:"docker"`
	Limits    Limits                   `yaml:"limits"`
	Sessions  SessionsConfig           `yaml:"sessions"`
	Profiles  map[string]ProfileConfig `yaml:"profiles"`
	Mounts    MountsConfig             `yaml:"mounts"`
	Workflows WorkflowsConfig          `yaml:"workflows"`
	Previews  PreviewsConfig           `yaml:"previews"`
	Memory    MemoryConfig             `yaml:"memory"`
	Webhooks  WebhooksConfig           `yaml:"webhooks"`
	// Integrations holds user-owned per-integration settings; a pack can
	// never grant itself these.
	Integrations map[string]IntegrationConfig `yaml:"integrations"`
	Usage        UsageConfig                  `yaml:"usage"`
	Update       UpdateConfig                 `yaml:"update"`
}

// DaemonConfig controls `icaro serve` and how clients reach it.
type DaemonConfig struct {
	// Listen adds a TCP listener (token required). Empty means Unix socket
	// only. It must be a loopback address unless AllowRemote is set: the
	// daemon speaks plain HTTP, so remote access belongs behind a
	// TLS-terminating reverse proxy on the same host.
	Listen string `yaml:"listen"`
	// AllowRemote lets Listen bind a non-loopback address (plain HTTP,
	// tokens in clear on the wire). Off by default (MVP cut 2).
	AllowRemote bool `yaml:"allow_remote"`
	// URL makes CLI clients use TCP instead of the local socket.
	URL string `yaml:"url"`
	// Token is the API token clients send over TCP.
	Token string `yaml:"token"`
	// UIListen is the loopback address `icaro ui` opens for the browser
	// (ADR 0023). It must be a loopback address.
	UIListen string `yaml:"ui_listen"`
	// Pprof serves net/http/pprof under /debug/pprof on the Unix socket
	// only (never TCP), for profiling (ADR 0024).
	Pprof bool `yaml:"pprof"`
	// PublicURL is the externally reachable base URL (webhook URLs).
	PublicURL string `yaml:"public_url"`
}

// DatabaseConfig selects the storage backend.
type DatabaseConfig struct {
	Driver string `yaml:"driver"` // sqlite | postgres
	DSN    string `yaml:"dsn"`    // empty for sqlite => <home>/config/icaro.db
}

// DockerConfig points at the engine.
type DockerConfig struct {
	Host string `yaml:"host"` // empty => DOCKER_HOST / default socket
	// StrictRuntime is an optional OCI runtime (e.g. runsc) for untrusted profiles.
	StrictRuntime string `yaml:"strict_runtime"`
	// HelperImage contains /icaro and runs Ícaro's helper containers (the
	// egress proxy). Empty: on Linux, built from the running binary.
	HelperImage string `yaml:"helper_image"`
}

// Limits are the resource ceilings applied to every container Ícaro
// creates. Nothing in a workflow can raise them.
type Limits struct {
	Timeout  time.Duration `yaml:"timeout"`
	MemoryMB int64         `yaml:"memory_mb"`
	CPUs     float64       `yaml:"cpus"`
	Pids     int64         `yaml:"pids"`
	NoFile   int64         `yaml:"nofile"`
}

// SessionsConfig governs the session spine.
type SessionsConfig struct {
	// MaxActive caps concurrently running sessions (O1); 0 derives it from RAM.
	MaxActive      int    `yaml:"max_active"`
	DefaultProfile string `yaml:"default_profile"`
	// HomeMode picks how profile homes are layered: auto | overlay | copy.
	HomeMode string `yaml:"home_mode"`
}

// ProfileConfig is one agent profile: image, agent CLI, egress allowlist
// and extra mounts. The egress allowlist is mandatory (ADR 0005).
type ProfileConfig struct {
	Agent   string   `yaml:"agent"`
	Image   string   `yaml:"image"`
	Egress  []string `yaml:"egress"`
	Mounts  []string `yaml:"mounts"`
	Runtime string   `yaml:"runtime"`
	// Tools are asdf plugin@version pairs materialized during setup.
	Tools []string `yaml:"tools"`
	// Inject maps a provider (InjectProviders) to the name of the secret
	// holding its API key. The key never enters the container: the agent
	// gets a base URL on the profile's egress proxy and a per-session
	// placeholder, and the daemon adds the key host-side (ADR 0028).
	Inject map[string]string `yaml:"inject,omitempty"`
}

// InjectProviders are the providers profiles.<name>.inject accepts.
var InjectProviders = []string{"anthropic", "openai"}

// secretNameRe mirrors secrets.ValidName (config cannot import secrets).
var secretNameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,127}$`)

// MountsConfig is the allowlist of host directories that may be mounted.
type MountsConfig struct {
	Allow []string `yaml:"allow"`
}

// WorkflowsConfig tunes the workflow engine.
type WorkflowsConfig struct {
	// Concurrency caps step containers running at once across all runs.
	Concurrency int `yaml:"concurrency"`
	// MaxRuns caps runs executing at once (0 = Concurrency); RunParallelism
	// caps parallel steps within one run (0 = only the global cap).
	MaxRuns        int    `yaml:"max_runs"`
	RunParallelism int    `yaml:"run_parallelism"`
	Profile        string `yaml:"profile"`
	// AllowHost permits image steps to run on the host (MVP cut 4: off).
	AllowHost bool `yaml:"allow_host"`
}

// PreviewsConfig governs per-worktree previews.
type PreviewsConfig struct {
	Enabled      bool          `yaml:"enabled"`
	Domain       string        `yaml:"domain"` // e.g. "test" for slug.test; empty => localhost:<port>
	HTTPPort     int           `yaml:"http_port"`
	TraefikImage string        `yaml:"traefik_image"`
	Debounce     time.Duration `yaml:"debounce"`
	// Roots are extra directories watched for compose projects, in
	// addition to the session worktrees.
	Roots []string `yaml:"roots"`
}

// MemoryConfig tunes shared memory.
type MemoryConfig struct {
	Preamble string `yaml:"preamble"`
}

// WebhooksConfig bounds inbound webhooks.
type WebhooksConfig struct {
	MaxBodyBytes int64         `yaml:"max_body_bytes"`
	MaxSkew      time.Duration `yaml:"max_skew"`
	RatePerMin   int           `yaml:"rate_per_min"`
	// Retention is how long delivery records are kept; it is also the
	// window in which a replayed delivery is recognized as a duplicate.
	Retention time.Duration `yaml:"retention"`
	// MaxRejected caps how many rejected (unverified) deliveries are kept.
	MaxRejected int `yaml:"max_rejected"`
}

// IntegrationConfig holds user opt-ins for one integration.
type IntegrationConfig struct {
	// AllowPrivate lets actions reach private-range addresses (self-hosted).
	AllowPrivate bool `yaml:"allow_private"`
	// BaseURL overrides a manifest's base URL for self-hosted services.
	BaseURL string `yaml:"base_url"`
}

// UsageConfig governs Phase 9 usage tracking.
type UsageConfig struct {
	Timezone   string  `yaml:"timezone"`
	DailyLimit float64 `yaml:"daily_limit_usd"`
}

// UpdateConfig controls self-update.
type UpdateConfig struct {
	BaseURL   string `yaml:"base_url"`
	PublicKey string `yaml:"public_key"` // base64 ed25519 release key
}

// DefaultProfiles are the built-in profiles. Their egress allowlists are
// locked to the agent provider's endpoints.
func DefaultProfiles() map[string]ProfileConfig {
	return map[string]ProfileConfig{
		"claude": {Agent: "claude", Image: DefaultAgentImage, Egress: []string{
			"api.anthropic.com", "console.anthropic.com", "claude.ai", "statsig.anthropic.com",
		}},
		"codex": {Agent: "codex", Image: DefaultAgentImage, Egress: []string{
			"api.openai.com", "auth.openai.com", "chatgpt.com",
		}},
		"shell":    {Agent: "", Image: DefaultAgentImage, Egress: []string{}},
		"workflow": {Agent: "", Image: DefaultAgentImage, Egress: []string{}},
	}
}

// Defaults returns the baseline configuration as flat keys.
func Defaults() map[string]any {
	return map[string]any{
		"version":                  CurrentVersion,
		"home":                     paths.Default().Root,
		"database.driver":          "sqlite",
		"daemon.ui_listen":         "127.0.0.1:8787",
		"limits.timeout":           "30m",
		"limits.memory_mb":         2048,
		"limits.cpus":              2.0,
		"limits.pids":              512,
		"limits.nofile":            4096,
		"sessions.max_active":      0,
		"sessions.default_profile": "claude",
		"workflows.concurrency":    4,
		"workflows.profile":        "workflow",
		"previews.enabled":         true,
		"previews.http_port":       8780,
		"previews.traefik_image":   "traefik:v3.1",
		"previews.debounce":        "2s",
		"memory.preamble":          DefaultPreamble,
		"webhooks.max_body_bytes":  1 << 20,
		"webhooks.max_skew":        "5m",
		"webhooks.rate_per_min":    120,
		"webhooks.retention":       "720h",
		"webhooks.max_rejected":    1000,
		"usage.timezone":           "Local",
	}
}

// DefaultPreamble asks agents to honor handoffs (architecture §8).
const DefaultPreamble = "Before you start, call the icaro memory tool `memory_query` with kind=handoff to accept any pending handoff for this branch. When you finish or stop, call `memory_write` with kind=handoff summarizing state, decisions and next steps."

// Load builds a Config from defaults, the config file and the
// environment. The file is the explicit path, else $ICARO_CONFIG, else
// <home>/config/icaro.yaml when it exists.
func Load(path string) (*Config, error) {
	tree := map[string]any{}
	for key, v := range Defaults() {
		setKey(tree, key, v)
	}
	resolved, err := resolveConfigPath(path)
	if err != nil {
		return nil, err
	}
	if resolved != "" {
		data, err := os.ReadFile(resolved)
		if err != nil {
			return nil, fmt.Errorf("config %s: %w", resolved, err)
		}
		var file map[string]any
		if err := yaml.Unmarshal(data, &file); err != nil {
			return nil, fmt.Errorf("config %s: %w", resolved, err)
		}
		merge(tree, file)
	}
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, envPrefix) {
			continue
		}
		if key := envKey(name); key != "" {
			setKey(tree, key, envValue(value))
		}
	}
	// The merged tree is decoded as YAML, so every layer gets the same
	// typing: durations parse from "30m", and an environment value is
	// typed by the field it lands in.
	data, err := yaml.Marshal(tree)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg.Home = expandHome(cfg.Home)
	profiles := DefaultProfiles()
	for name, p := range cfg.Profiles {
		base, ok := profiles[name]
		if ok {
			if p.Agent == "" {
				p.Agent = base.Agent
			}
			if p.Image == "" {
				p.Image = base.Image
			}
			if p.Egress == nil {
				p.Egress = base.Egress
			}
		}
		if p.Image == "" {
			p.Image = DefaultAgentImage
		}
		profiles[name] = p
	}
	cfg.Profiles = profiles
	return &cfg, nil
}

// setKey sets a dotted key (daemon.listen) in a nested tree.
func setKey(tree map[string]any, key string, v any) {
	parts := strings.Split(key, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := tree[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			tree[p] = next
		}
		tree = next
	}
	tree[parts[len(parts)-1]] = v
}

// merge lays src over dst: maps merge key by key, anything else (lists
// included) replaces.
func merge(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				merge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

// envValue is an environment value, emitted as an untagged scalar so it
// decodes as the field's type: "7" into an int, "true" into a bool, and
// "2s" into a duration.
type envValue string

func (v envValue) MarshalYAML() (any, error) {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: string(v)}, nil
}

// envKey maps ICARO_DAEMON__LISTEN to daemon.listen. ICARO_HOME,
// ICARO_CONFIG and ICARO_MASTER_KEY are not config keys.
func envKey(s string) string {
	s = strings.TrimPrefix(s, envPrefix)
	switch s {
	case "CONFIG", "MASTER_KEY", "PASSWORD", "TEST_POSTGRES_DSN", "SESSION", "DOCKER_TESTS":
		return ""
	}
	return strings.ReplaceAll(strings.ToLower(s), "__", ".")
}

var profileNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Validate checks invariants that cannot be expressed structurally.
func (c *Config) Validate() error {
	var errs []error
	if c.Version > CurrentVersion || c.Version < 1 {
		errs = append(errs, fmt.Errorf("config version %d is not supported by this binary (supports %d); upgrade icaro", c.Version, CurrentVersion))
	}
	if c.Home == "" {
		errs = append(errs, errors.New("home is required"))
	}
	switch c.Database.Driver {
	case "sqlite", "postgres":
	default:
		errs = append(errs, fmt.Errorf("database.driver must be sqlite or postgres, got %q", c.Database.Driver))
	}
	if c.Database.Driver == "postgres" && c.Database.DSN == "" {
		errs = append(errs, errors.New("database.dsn is required for postgres"))
	}
	if c.Daemon.RemoteListen() && !c.Daemon.AllowRemote {
		errs = append(errs, fmt.Errorf("daemon.listen %q is not a loopback address; the daemon speaks plain HTTP, so listen on 127.0.0.1 behind a TLS-terminating reverse proxy, or set daemon.allow_remote: true (--allow-remote) to accept the risk", c.Daemon.Listen))
	}
	if c.Daemon.UIListen != "" && !isLoopback(c.Daemon.UIListen) {
		errs = append(errs, fmt.Errorf("daemon.ui_listen %q must be a loopback address (use --listen for anything else)", c.Daemon.UIListen))
	}
	if c.Workflows.Concurrency < 1 {
		errs = append(errs, errors.New("workflows.concurrency must be >= 1"))
	}
	if c.Workflows.MaxRuns < 0 {
		errs = append(errs, errors.New("workflows.max_runs must be >= 0 (0 = workflows.concurrency)"))
	}
	if c.Workflows.RunParallelism < 0 {
		errs = append(errs, errors.New("workflows.run_parallelism must be >= 0 (0 = no per-run cap)"))
	}
	if c.Limits.Timeout <= 0 {
		errs = append(errs, errors.New("limits.timeout must be > 0"))
	}
	if c.Sessions.MaxActive < 0 {
		errs = append(errs, errors.New("sessions.max_active must be >= 0"))
	}
	for name, p := range c.Profiles {
		if !profileNameRe.MatchString(name) {
			errs = append(errs, fmt.Errorf("profile name %q must match %s", name, profileNameRe))
		}
		if p.Egress == nil {
			errs = append(errs, fmt.Errorf("profiles.%s.egress is mandatory (use [] for no network)", name))
		}
		for _, h := range p.Egress {
			if strings.ContainsAny(h, "/: ") {
				errs = append(errs, fmt.Errorf("profiles.%s.egress: %q must be a hostname or *.domain", name, h))
			}
		}
		for prov, secret := range p.Inject {
			if !slices.Contains(InjectProviders, prov) {
				errs = append(errs, fmt.Errorf("profiles.%s.inject: unknown provider %q (known: %s)", name, prov, strings.Join(InjectProviders, ", ")))
			}
			if !secretNameRe.MatchString(secret) {
				errs = append(errs, fmt.Errorf("profiles.%s.inject.%s: %q is not a secret name (set one with icaro secret set)", name, prov, secret))
			}
		}
	}
	if _, ok := c.Profiles[c.Sessions.DefaultProfile]; !ok {
		errs = append(errs, fmt.Errorf("sessions.default_profile %q is not a profile", c.Sessions.DefaultProfile))
	}
	if _, ok := c.Profiles[c.Workflows.Profile]; !ok {
		errs = append(errs, fmt.Errorf("workflows.profile %q is not a profile", c.Workflows.Profile))
	}
	return errors.Join(errs...)
}

// Layout returns the on-disk layout for this config.
func (c *Config) Layout() paths.Layout { return paths.New(c.Home) }

// SQLitePath is the database file used when the driver is sqlite.
func (c *Config) SQLitePath() string {
	if c.Database.DSN != "" {
		return c.Database.DSN
	}
	return c.Layout().DB()
}

// LogsDir is where step logs are written.
func (c *Config) LogsDir() string { return c.Layout().Logs() }

// ResolvedPath returns the config file Load would read, or "".
func ResolvedPath(explicit string) string {
	p, _ := resolveConfigPath(explicit)
	return p
}

func resolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("config file: %w", err)
		}
		return explicit, nil
	}
	if v := os.Getenv("ICARO_CONFIG"); v != "" {
		if _, err := os.Stat(v); err != nil {
			return "", fmt.Errorf("ICARO_CONFIG: %w", err)
		}
		return v, nil
	}
	p := paths.Default().ConfigFile()
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	return "", nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

// RemoteListen reports whether Listen binds something other than a
// loopback address (":8787" binds every interface).
func (d DaemonConfig) RemoteListen() bool {
	return d.Listen != "" && !isLoopback(d.Listen)
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
