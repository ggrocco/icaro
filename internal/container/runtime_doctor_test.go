package container

import (
	"context"
	"errors"
	"strings"
	"testing"

	"icaro/internal/doctor"
)

type runtimeFake struct {
	ping, list error
	runtimes   []string
}

func (f *runtimeFake) Ping(context.Context) error { return f.ping }
func (f *runtimeFake) Runtimes(context.Context) ([]string, error) {
	return f.runtimes, f.list
}

func TestRuntimeCheck(t *testing.T) {
	ctx := context.Background()
	f := &runtimeFake{ping: errors.New("down")}
	if r := RuntimeCheck(f, nil).Run(ctx); r.Severity != doctor.OK || !strings.Contains(r.Detail, "default runtime") {
		t.Fatalf("no runtime configured needs no engine: %+v", r)
	}
	c := RuntimeCheck(f, map[string]string{"claude": "runsc", "workflow": "runsc", "codex": "kata"})
	if r := c.Run(ctx); r.Severity != doctor.Skipped {
		t.Fatalf("unreachable: %+v", r)
	}
	f.ping, f.list = nil, errors.New("boom")
	if r := c.Run(ctx); r.Severity != doctor.Error || r.Detail != "boom" {
		t.Fatalf("info error: %+v", r)
	}
	f.list, f.runtimes = nil, []string{"io.containerd.runc.v2", "runc", "runsc"}
	r := c.Run(ctx)
	if r.Severity != doctor.Error || !strings.Contains(r.Detail, `codex uses "kata"`) || strings.Contains(r.Detail, "claude") ||
		!strings.Contains(r.Detail, "registered: io.containerd.runc.v2, runc, runsc") || !strings.Contains(r.Detail, "runsc install") {
		t.Fatalf("missing runtime: %+v", r)
	}
	f.runtimes = append(f.runtimes, "kata")
	if r := c.Run(ctx); r.Severity != doctor.OK || r.Detail != "claude: runsc, codex: kata, workflow: runsc" {
		t.Fatalf("all registered: %+v", r)
	}
}
