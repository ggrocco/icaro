package autostart

import (
	"context"
	"encoding/xml"
	"errors"
	"os"
	"strings"
	"testing"
)

type fakeRun struct {
	calls []string
	fail  map[string]bool
	out   map[string]string
}

func (f *fakeRun) run(_ context.Context, name string, args ...string) ([]byte, error) {
	c := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, c)
	for k := range f.fail {
		if strings.HasPrefix(c, k) {
			return []byte("nope"), errors.New("exit 1")
		}
	}
	for k, v := range f.out {
		if strings.HasPrefix(c, k) {
			return []byte(v), nil
		}
	}
	return nil, nil
}

func newManager(t *testing.T, goos string) (*Manager, *fakeRun) {
	f := &fakeRun{fail: map[string]bool{}, out: map[string]string{}}
	return &Manager{
		GOOS: goos, UserDir: t.TempDir(), UID: 501, Exe: "/opt/icaro dir/icaro", Args: []string{"--home", "/Users/a/.icaro"},
		Home: "/Users/a/.icaro", LogFile: "/Users/a/.icaro/logs/daemon.log", Run: f.run,
	}, f
}

func TestLaunchdPlist(t *testing.T) {
	m, f := newManager(t, "darwin")
	u, err := m.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(u.Path, "Library/LaunchAgents/dev.icaro.daemon.plist") {
		t.Fatal(u.Path)
	}
	body := string(u.Content)
	for _, want := range []string{"<string>/opt/icaro dir/icaro</string>", "<string>--home</string>", "<string>serve</string>", "<key>RunAtLoad</key>\n  <true/>", "<key>SuccessfulExit</key>", "daemon.log"} {
		if !strings.Contains(body, want) {
			t.Fatalf("plist missing %q:\n%s", want, body)
		}
	}
	// Well-formed XML.
	dec := xml.NewDecoder(strings.NewReader(body))
	dec.Strict = false
	for {
		if _, err := dec.Token(); err != nil {
			if err.Error() != "EOF" {
				t.Fatalf("plist is not XML: %v", err)
			}
			break
		}
	}
	if got := strings.Join(f.calls, "\n"); !strings.Contains(got, "launchctl bootstrap gui/501 "+u.Path) {
		t.Fatalf("calls: %s", got)
	}
	f.out["launchctl print"] = "state = running"
	st, _ := m.Status(context.Background())
	if !st.Installed || !st.Active {
		t.Fatalf("%+v", st)
	}
	if err := m.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(u.Path); !os.IsNotExist(err) {
		t.Fatal("plist not removed")
	}
}

func TestSystemdUnit(t *testing.T) {
	m, f := newManager(t, "linux")
	m.Args = []string{"--config", `/tmp/50% "odd".yaml`}
	u, err := m.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body := string(u.Content)
	for _, want := range []string{
		`ExecStart="/opt/icaro dir/icaro" --config "/tmp/50%% \"odd\".yaml" serve`,
		"Environment=ICARO_HOME=/Users/a/.icaro", "RestartPreventExitStatus=3", "WantedBy=default.target",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("unit missing %q:\n%s", want, body)
		}
	}
	if !strings.HasSuffix(u.Path, ".config/systemd/user/icaro.service") {
		t.Fatal(u.Path)
	}
	if got := strings.Join(f.calls, "\n"); !strings.Contains(got, "systemctl --user enable --now icaro.service") {
		t.Fatal(got)
	}
	f.out["systemctl --user is-active"] = "active\n"
	if st, _ := m.Status(context.Background()); !st.Active || !st.Installed {
		t.Fatalf("%+v", st)
	}
	if err := m.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Uninstall(context.Background()); err != nil {
		t.Fatal("uninstall must be idempotent")
	}
}

func TestErrors(t *testing.T) {
	m, f := newManager(t, "linux")
	f.fail["systemctl --user enable"] = true
	if _, err := m.Install(context.Background()); err == nil {
		t.Fatal("enable failure must surface")
	}
	m.Exe = "relative/icaro"
	if _, err := m.Unit(); err == nil {
		t.Fatal("relative exe accepted")
	}
	w, _ := newManager(t, "windows")
	if _, err := w.Unit(); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if _, err := w.Status(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	d, df := newManager(t, "darwin")
	df.fail["launchctl bootstrap"] = true
	if _, err := d.Install(context.Background()); err == nil {
		t.Fatal("bootstrap failure must surface")
	}
	if systemdQuote("plain") != "plain" || systemdQuote("") != `""` {
		t.Fatal("quote")
	}
}
