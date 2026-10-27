// Package autostart installs the daemon as a per-user login service: a
// launchd user agent on macOS and a systemd user unit on Linux (Phase 7,
// "launch at login"). The unit runs `icaro serve` in the foreground and
// lets the service manager restart it; a duplicate daemon exits with
// status 3, which the units treat as "do not restart".
package autostart

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

// Label is the launchd label and systemd unit base name.
const (
	Label       = "dev.icaro.daemon"
	SystemdUnit = "icaro.service"
)

// Runner executes a service-manager command (edge; faked in tests).
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs real commands.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // fixed service-manager commands
}

// Manager installs and removes the login service.
type Manager struct {
	GOOS    string
	UserDir string   // the user's home directory
	UID     int      // for launchctl domains
	Exe     string   // absolute path of the icaro binary
	Args    []string // global flags placed before "serve" (e.g. --home)
	Home    string   // ICARO_HOME passed to the daemon
	LogFile string   // stdout/stderr target on macOS
	Run     Runner
}

// Unit is a generated service definition.
type Unit struct {
	Path    string
	Content []byte
}

// Status describes the installed service.
type Status struct {
	Installed bool   `json:"installed"`
	Path      string `json:"path"`
	Active    bool   `json:"active"`
	Detail    string `json:"detail,omitempty"`
}

// ErrUnsupported reports a platform without a supported service manager.
var ErrUnsupported = errors.New("launch at login is supported on macOS (launchd) and Linux (systemd --user)")

// Path returns where the unit file lives.
func (m *Manager) Path() (string, error) {
	switch m.GOOS {
	case "darwin":
		return filepath.Join(m.UserDir, "Library", "LaunchAgents", Label+".plist"), nil
	case "linux":
		return filepath.Join(m.UserDir, ".config", "systemd", "user", SystemdUnit), nil
	}
	return "", ErrUnsupported
}

// Unit renders the service definition.
func (m *Manager) Unit() (Unit, error) {
	if !filepath.IsAbs(m.Exe) {
		return Unit{}, fmt.Errorf("executable path %q must be absolute", m.Exe)
	}
	p, err := m.Path()
	if err != nil {
		return Unit{}, err
	}
	argv := append(append([]string{m.Exe}, m.Args...), "serve")
	var buf bytes.Buffer
	switch m.GOOS {
	case "darwin":
		err = plistTmpl.Execute(&buf, map[string]any{
			"Label": Label, "Argv": argv, "Home": m.Home, "Log": m.LogFile,
			"Path": "/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		})
	case "linux":
		quoted := make([]string, len(argv))
		for i, a := range argv {
			quoted[i] = systemdQuote(a)
		}
		err = unitTmpl.Execute(&buf, map[string]any{"ExecStart": strings.Join(quoted, " "), "Env": systemdQuote("ICARO_HOME=" + m.Home)})
	}
	if err != nil {
		return Unit{}, err
	}
	return Unit{Path: p, Content: buf.Bytes()}, nil
}

// Install writes the unit and activates it.
func (m *Manager) Install(ctx context.Context) (Unit, error) {
	u, err := m.Unit()
	if err != nil {
		return u, err
	}
	if err := os.MkdirAll(filepath.Dir(u.Path), 0o750); err != nil {
		return u, err
	}
	if err := os.WriteFile(u.Path, u.Content, 0o644); err != nil { //nolint:gosec // service managers require readable units
		return u, err
	}
	switch m.GOOS {
	case "darwin":
		// Re-installing replaces a loaded agent.
		_, _ = m.Run(ctx, "launchctl", "bootout", m.domain()+"/"+Label)
		if out, err := m.Run(ctx, "launchctl", "bootstrap", m.domain(), u.Path); err != nil {
			return u, fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
		}
	case "linux":
		if out, err := m.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return u, fmt.Errorf("systemctl daemon-reload: %v: %s", err, out)
		}
		if out, err := m.Run(ctx, "systemctl", "--user", "enable", "--now", SystemdUnit); err != nil {
			return u, fmt.Errorf("systemctl enable: %v: %s", err, out)
		}
	}
	return u, nil
}

// Uninstall deactivates and removes the unit. Missing units are fine.
func (m *Manager) Uninstall(ctx context.Context) error {
	p, err := m.Path()
	if err != nil {
		return err
	}
	switch m.GOOS {
	case "darwin":
		_, _ = m.Run(ctx, "launchctl", "bootout", m.domain()+"/"+Label)
	case "linux":
		_, _ = m.Run(ctx, "systemctl", "--user", "disable", "--now", SystemdUnit)
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if m.GOOS == "linux" {
		_, _ = m.Run(ctx, "systemctl", "--user", "daemon-reload")
	}
	return nil
}

// Status reports whether the unit is installed and active.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	p, err := m.Path()
	if err != nil {
		return Status{}, err
	}
	st := Status{Path: p}
	if _, err := os.Stat(p); err == nil {
		st.Installed = true
	}
	switch m.GOOS {
	case "darwin":
		out, err := m.Run(ctx, "launchctl", "print", m.domain()+"/"+Label)
		st.Active = err == nil && strings.Contains(string(out), "state = running")
	case "linux":
		out, err := m.Run(ctx, "systemctl", "--user", "is-active", SystemdUnit)
		st.Detail = strings.TrimSpace(string(out))
		st.Active = err == nil && st.Detail == "active"
	}
	return st, nil
}

// Restart restarts the daemon through the service manager, so it stays
// supervised: stopping it with a signal exits 0, which neither
// Restart=on-failure nor KeepAlive/SuccessfulExit=false restarts.
func (m *Manager) Restart(ctx context.Context) error {
	var name string
	var args []string
	switch m.GOOS {
	case "darwin":
		name, args = "launchctl", []string{"kickstart", "-k", m.domain() + "/" + Label}
	case "linux":
		name, args = "systemctl", []string{"--user", "restart", SystemdUnit}
	default:
		return ErrUnsupported
	}
	if out, err := m.Run(ctx, name, args...); err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
	return nil
}

func (m *Manager) domain() string { return fmt.Sprintf("gui/%d", m.UID) }

// systemdQuote quotes one ExecStart/Environment word.
func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if s != "" && !strings.ContainsAny(s, " \t\"'\\$;") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`)
	return `"` + r.Replace(s) + `"`
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

var plistTmpl = template.Must(template.New("plist").Funcs(template.FuncMap{"x": xmlEscape}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>{{x .Label}}</string>
  <key>ProgramArguments</key>
  <array>
{{- range .Argv}}
    <string>{{x .}}</string>
{{- end}}
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>ICARO_HOME</key>
    <string>{{x .Home}}</string>
    <key>PATH</key>
    <string>{{x .Path}}</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ThrottleInterval</key>
  <integer>30</integer>
  <key>ProcessType</key>
  <string>Background</string>
{{- if .Log}}
  <key>StandardOutPath</key>
  <string>{{x .Log}}</string>
  <key>StandardErrorPath</key>
  <string>{{x .Log}}</string>
{{- end}}
</dict>
</plist>
`))

var unitTmpl = template.Must(template.New("unit").Parse(`[Unit]
Description=Ícaro daemon (parallel isolated coding agents)
Documentation=https://github.com/ggrocco/icaro

[Service]
Type=simple
ExecStart={{.ExecStart}}
Environment={{.Env}}
Restart=on-failure
RestartSec=5
# 3 = another daemon already holds the lock; restarting cannot help.
RestartPreventExitStatus=3
# The daemon tightens its own file modes; keep new files private.
UMask=0077

[Install]
WantedBy=default.target
`))
