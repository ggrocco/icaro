// Package doctor runs health checks and their remediations
// (03-security, "icaro doctor"). Each component contributes Check values;
// the CLI runs them, prints results, and applies fixes with --fix.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"icaro/internal/daemon"
	"icaro/internal/paths"
)

// Severity of a failed check.
type Severity string

// Severities.
const (
	OK      Severity = "ok"
	Warning Severity = "warning"
	Error   Severity = "error"
	Skipped Severity = "skipped"
)

// Result is the outcome of one check.
type Result struct {
	Severity Severity
	Detail   string
	// Fix remediates the problem; nil when there is nothing to fix
	// automatically. It returns a description of what it did.
	Fix func(ctx context.Context) (string, error)
}

// Check is one doctor check.
type Check struct {
	Name string
	Run  func(ctx context.Context) Result
}

// Report is one executed check.
type Report struct {
	Name     string   `json:"name"`
	Severity Severity `json:"severity"`
	Detail   string   `json:"detail"`
	Fixed    string   `json:"fixed,omitempty"`
	FixError string   `json:"fix_error,omitempty"`
}

// Run executes checks in order; with fix, failing checks that offer a Fix
// are remediated and re-run.
func Run(ctx context.Context, checks []Check, fix bool) []Report {
	out := make([]Report, 0, len(checks))
	for _, c := range checks {
		res := c.Run(ctx)
		rep := Report{Name: c.Name, Severity: res.Severity, Detail: res.Detail}
		if fix && res.Fix != nil && (res.Severity == Warning || res.Severity == Error) {
			did, err := res.Fix(ctx)
			if err != nil {
				rep.FixError = err.Error()
			} else {
				rep.Fixed = did
				again := c.Run(ctx)
				rep.Severity, rep.Detail = again.Severity, again.Detail
			}
		}
		out = append(out, rep)
	}
	return out
}

// Worst returns the most severe result.
func Worst(reports []Report) Severity {
	worst := OK
	for _, r := range reports {
		switch {
		case r.Severity == Error:
			return Error
		case r.Severity == Warning:
			worst = Warning
		}
	}
	return worst
}

// Print renders reports for humans.
func Print(w io.Writer, reports []Report) {
	for _, r := range reports {
		mark := map[Severity]string{OK: "ok  ", Warning: "warn", Error: "FAIL", Skipped: "skip"}[r.Severity]
		fmt.Fprintf(w, "[%s] %s: %s\n", mark, r.Name, r.Detail)
		if r.Fixed != "" {
			fmt.Fprintf(w, "       fixed: %s\n", r.Fixed)
		}
		if r.FixError != "" {
			fmt.Fprintf(w, "       fix failed: %s\n", r.FixError)
		}
	}
}

// DaemonCheck detects an orphaned lock/socket/PID: files left by a daemon
// that is no longer running, or a daemon holding the lock but not answering.
func DaemonCheck(l paths.Layout) Check {
	return Check{Name: "daemon lock/socket/pid", Run: func(ctx context.Context) Result {
		st, err := daemon.Probe(ctx, l)
		if err != nil {
			return Result{Severity: Error, Detail: err.Error()}
		}
		switch {
		case st.Running && st.SocketOK:
			return Result{Severity: OK, Detail: fmt.Sprintf("daemon running (pid %d)", st.PID)}
		case st.Running:
			return Result{Severity: Error, Detail: fmt.Sprintf("lock held (pid %d) but the socket does not answer; stop that process and re-run", st.PID)}
		case st.Orphaned():
			return Result{Severity: Warning, Detail: "stale pid/socket from a daemon that is no longer running", Fix: func(ctx context.Context) (string, error) {
				removed, err := daemon.CleanOrphans(ctx, l)
				return fmt.Sprintf("removed %v", removed), err
			}}
		default:
			return Result{Severity: OK, Detail: "no daemon running, no leftovers"}
		}
	}}
}

// PermissionsCheck enforces 0700 on directories and 0600 on files under
// the state root.
func PermissionsCheck(l paths.Layout) Check {
	return Check{Name: "permissions on " + l.Root, Run: func(context.Context) Result {
		bad, err := insecure(l.Root)
		if errors.Is(err, os.ErrNotExist) {
			return Result{Severity: OK, Detail: "state root not created yet"}
		}
		if err != nil {
			return Result{Severity: Error, Detail: err.Error()}
		}
		if len(bad) == 0 {
			return Result{Severity: OK, Detail: "0700 dirs / 0600 files"}
		}
		sort.Strings(bad)
		detail := fmt.Sprintf("%d path(s) readable by others, e.g. %s", len(bad), bad[0])
		return Result{Severity: Error, Detail: detail, Fix: func(context.Context) (string, error) {
			for _, p := range bad {
				fi, err := os.Lstat(p)
				if err != nil {
					continue
				}
				mode := os.FileMode(0o600)
				if fi.IsDir() {
					mode = 0o700
				}
				if err := os.Chmod(p, mode); err != nil {
					return "", err
				}
			}
			return fmt.Sprintf("tightened %d path(s)", len(bad)), nil
		}}
	}}
}

// insecure lists paths with group/other permission bits. Worktrees, homes
// and repos hold user content whose modes are the user's business below
// their top level.
func insecure(root string) ([]string, error) {
	if _, err := os.Stat(root); err != nil {
		return nil, err
	}
	skipBelow := map[string]bool{}
	l := paths.New(root)
	for _, d := range []string{l.Worktrees(), l.Homes(), l.Repos(), l.Runs(), l.Integrations()} {
		skipBelow[d] = true
	}
	var bad []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if fi.Mode().Perm()&0o077 != 0 && fi.Mode()&fs.ModeSocket == 0 {
			bad = append(bad, p)
		}
		if d.IsDir() && skipBelow[p] {
			return fs.SkipDir
		}
		return nil
	})
	return bad, err
}

// Func adapts a simple function into a Check.
func Func(name string, fn func(ctx context.Context) Result) Check {
	return Check{Name: name, Run: fn}
}
