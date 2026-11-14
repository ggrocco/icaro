package memory

import (
	"context"
	"fmt"

	"icaro/internal/store"
)

// ByOrigin lists the live memories one origin wrote in the caller's
// project, newest first.
func (e *Engine) ByOrigin(ctx context.Context, scope Scope, origin string, limit int) ([]*Memory, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := e.db.ListMemories(ctx, store.MemoryFilter{ProjectID: scope.ProjectID, Origin: origin, Now: e.now(), Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]*Memory, 0, len(rows))
	for _, r := range rows {
		out = append(out, toMemory(r, true))
	}
	return out, nil
}

// Remove deletes memories of the caller's project by id; ids of other
// projects are ignored.
func (e *Engine) Remove(ctx context.Context, scope Scope, ids ...string) error {
	if err := scope.check(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return fmt.Errorf("%w: no memory ids", ErrInvalid)
	}
	return e.db.DeleteMemories(ctx, scope.ProjectID, ids)
}

// StableID is the id Put gives an item with key under origin in project.
func StableID(project, origin, key string) string { return stableID(project, origin, key) }
