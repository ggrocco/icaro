package session

import (
	"context"
	"fmt"
	"strings"
	"time"

	"icaro/internal/store"
)

// LoginLease bounds how long a login holds its profile when the CLI never
// reports its end (it crashed, or lost the daemon).
const LoginLease = 30 * time.Minute

// BeginLogin reserves profile for a login. While the lease is held, no
// session of the profile starts: queued ones wait, so nothing reads the
// seed the login is rewriting. The lease is taken before the busy check,
// so a session that starts in between is seen by the check. EndLogin
// releases it; force skips the busy check and replaces another login's
// lease.
func (m *Manager) BeginLogin(ctx context.Context, profile string, force bool) error {
	held := false
	m.logins.do(func(l map[string]time.Time) {
		if end, ok := l[profile]; ok && time.Now().Before(end) && !force {
			held = true
			return
		}
		l[profile] = time.Now().Add(LoginLease)
	})
	if held {
		return &ErrBusy{Msg: fmt.Sprintf("a login of profile %s is already in progress; wait for it to finish, or pass --force", profile)}
	}
	if err := m.CheckLogin(ctx, profile, force); err != nil {
		m.EndLogin(profile)
		return err
	}
	return nil
}

// EndLogin releases the login lease of profile and starts what waited.
func (m *Manager) EndLogin(profile string) {
	m.logins.do(func(l map[string]time.Time) { delete(l, profile) })
	m.nudge()
}

// loginHeld reports whether a login holds profile.
func (m *Manager) loginHeld(profile string) bool {
	held := false
	m.logins.do(func(l map[string]time.Time) {
		end, ok := l[profile]
		held = ok && time.Now().Before(end)
		if ok && !held {
			delete(l, profile)
		}
	})
	return held
}

// ErrBusy reports an operation refused because sessions are using what it
// would change.
type ErrBusy struct{ Msg string }

func (e *ErrBusy) Error() string { return e.Msg }

// CheckLogin refuses a login for profile while one of its sessions may be
// using the profile seed, which the login rewrites in place. In overlay
// mode the seed is the read-only lower layer of every running session's
// home, and changing it under a mounted overlay is undefined; in both
// modes a creating or queued session reads the seed when it starts. force
// skips the check.
func (m *Manager) CheckLogin(ctx context.Context, profile string, force bool) error {
	if force {
		return nil
	}
	statuses := []string{StatusCreating, StatusQueued}
	if m.o.Homes != nil && m.o.Homes.OverlayEnabled() {
		statuses = append(statuses, StatusRunning)
	}
	rows, err := m.o.Store.ListSessions(ctx, store.SessionFilter{Statuses: statuses})
	if err != nil {
		return err
	}
	var busy []string
	for _, r := range rows {
		if r.Profile == profile {
			busy = append(busy, r.ID+" ("+r.Status+")")
		}
	}
	if len(busy) == 0 {
		return nil
	}
	return &ErrBusy{Msg: fmt.Sprintf("profile %s has sessions using its home seed: %s; stop them first (`icaro session stop ID`), or pass --force to log in anyway", profile, strings.Join(busy, ", "))}
}
