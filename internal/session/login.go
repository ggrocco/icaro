package session

import (
	"context"
	"fmt"
	"strings"

	"icaro/internal/store"
)

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
