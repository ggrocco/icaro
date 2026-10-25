package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Project is a host git repository that sessions work on.
type Project struct {
	ID        string
	Name      string
	RootPath  string
	Remote    string
	CreatedAt time.Time
}

// SessionRow is the persisted session (ADR 0006).
type SessionRow struct {
	ID                string
	Kind              string
	ProjectID         string
	Profile           string
	Branch            string
	WorktreePath      string
	ContainerID       string
	ImageDigest       string
	PreviewInstanceID string
	RunID             string
	Status            string
	Error             string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	LastHeartbeat     *time.Time
}

// SessionFilter narrows ListSessions.
type SessionFilter struct {
	ProjectID string
	Kind      string
	Statuses  []string
}

// UpsertProject inserts a project by root path or returns the existing one.
func (s *Store) UpsertProject(ctx context.Context, p *Project) (*Project, error) {
	if existing, err := s.GetProjectByPath(ctx, p.RootPath); err == nil {
		if p.Remote != "" && existing.Remote != p.Remote {
			if _, err := s.exec(ctx, `UPDATE projects SET remote = ? WHERE id = ?`, p.Remote, existing.ID); err != nil {
				return nil, err
			}
			existing.Remote = p.Remote
		}
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if p.ID == "" {
		p.ID = NewID()
	}
	p.CreatedAt = Now()
	_, err := s.exec(ctx, `INSERT INTO projects (id, name, root_path, remote, created_at) VALUES (?,?,?,?,?)`,
		p.ID, p.Name, p.RootPath, p.Remote, ts(p.CreatedAt))
	if isUniqueViolation(err) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

const projectCols = `id, name, root_path, remote, created_at`

func scanProject(sc interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	var created string
	err := sc.Scan(&p.ID, &p.Name, &p.RootPath, &p.Remote, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt = parseTS(created)
	return &p, nil
}

// GetProject fetches a project by id or name.
func (s *Store) GetProject(ctx context.Context, idOrName string) (*Project, error) {
	return scanProject(s.queryRow(ctx, `SELECT `+projectCols+` FROM projects WHERE id = ? OR name = ?`, idOrName, idOrName))
}

// GetProjectByPath fetches a project by its root path.
func (s *Store) GetProjectByPath(ctx context.Context, root string) (*Project, error) {
	return scanProject(s.queryRow(ctx, `SELECT `+projectCols+` FROM projects WHERE root_path = ?`, root))
}

// ListProjects returns projects by name.
func (s *Store) ListProjects(ctx context.Context) ([]*Project, error) {
	rows, err := s.query(ctx, `SELECT `+projectCols+` FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

const sessionCols = `id, kind, project_id, profile, branch, worktree_path, container_id, image_digest, preview_instance_id, run_id, status, error, created_at, updated_at, last_heartbeat`

func scanSession(sc interface{ Scan(...any) error }) (*SessionRow, error) {
	var r SessionRow
	var created, updated string
	var hb sql.NullString
	err := sc.Scan(&r.ID, &r.Kind, &r.ProjectID, &r.Profile, &r.Branch, &r.WorktreePath, &r.ContainerID, &r.ImageDigest,
		&r.PreviewInstanceID, &r.RunID, &r.Status, &r.Error, &created, &updated, &hb)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt, r.UpdatedAt, r.LastHeartbeat = parseTS(created), parseTS(updated), parseTSPtr(hb)
	return &r, nil
}

// CreateSession inserts a session; ID and timestamps are filled in.
func (s *Store) CreateSession(ctx context.Context, r *SessionRow) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	now := Now()
	r.CreatedAt, r.UpdatedAt = now, now
	_, err := s.exec(ctx, `INSERT INTO sessions (`+sessionCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Kind, r.ProjectID, r.Profile, r.Branch, r.WorktreePath, r.ContainerID, r.ImageDigest,
		r.PreviewInstanceID, r.RunID, r.Status, r.Error, ts(now), ts(now), tsPtr(r.LastHeartbeat))
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

const sessionSet = `UPDATE sessions SET profile = ?, branch = ?, worktree_path = ?, container_id = ?, image_digest = ?,
		preview_instance_id = ?, run_id = ?, status = ?, error = ?, updated_at = ?, last_heartbeat = ? WHERE id = ?`

func sessionArgs(r *SessionRow) []any {
	return []any{r.Profile, r.Branch, r.WorktreePath, r.ContainerID, r.ImageDigest, r.PreviewInstanceID, r.RunID, r.Status, r.Error,
		ts(r.UpdatedAt), tsPtr(r.LastHeartbeat), r.ID}
}

// UpdateSession writes every mutable column.
func (s *Store) UpdateSession(ctx context.Context, r *SessionRow) error {
	r.UpdatedAt = Now()
	res, err := s.exec(ctx, sessionSet, sessionArgs(r)...)
	if err != nil {
		return err
	}
	return affected(res)
}

// UpdateSessionIf writes every mutable column only while the stored
// status is one of from, and reports whether it did. A false result
// means another writer moved the session on (or deleted it) since r was
// read: the compare-and-set that keeps a stop from being overwritten by
// a start still in flight.
func (s *Store) UpdateSessionIf(ctx context.Context, r *SessionRow, from ...string) (bool, error) {
	if len(from) == 0 {
		return false, errors.New("update session: no expected status")
	}
	r.UpdatedAt = Now()
	args := sessionArgs(r)
	for _, f := range from {
		args = append(args, f)
	}
	res, err := s.exec(ctx, sessionSet+` AND status IN (`+strings.TrimSuffix(strings.Repeat("?,", len(from)), ",")+`)`, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// GetSession fetches a session by id.
func (s *Store) GetSession(ctx context.Context, id string) (*SessionRow, error) {
	return scanSession(s.queryRow(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id))
}

// GetSessionByRun fetches the run session of a workflow run.
func (s *Store) GetSessionByRun(ctx context.Context, runID string) (*SessionRow, error) {
	return scanSession(s.queryRow(ctx, `SELECT `+sessionCols+` FROM sessions WHERE run_id = ? AND kind = 'run' ORDER BY created_at DESC LIMIT 1`, runID))
}

// ListSessions returns sessions oldest first.
func (s *Store) ListSessions(ctx context.Context, f SessionFilter) ([]*SessionRow, error) {
	var where []string
	var args []any
	if f.ProjectID != "" {
		where = append(where, "project_id = ?")
		args = append(args, f.ProjectID)
	}
	if f.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, f.Kind)
	}
	if len(f.Statuses) > 0 {
		where = append(where, "status IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.Statuses)), ",")+")")
		for _, st := range f.Statuses {
			args = append(args, st)
		}
	}
	q := `SELECT ` + sessionCols + ` FROM sessions`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY created_at, id"
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*SessionRow
	for rows.Next() {
		r, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountSessions counts sessions of a kind in the given statuses ("" kind = any).
func (s *Store) CountSessions(ctx context.Context, kind string, statuses ...string) (int, error) {
	rows, err := s.ListSessions(ctx, SessionFilter{Kind: kind, Statuses: statuses})
	return len(rows), err
}

// TransitionSession sets status only when the current status is one of
// from; it reports whether the row changed. This is the claim used to
// start a queued session exactly once.
func (s *Store) TransitionSession(ctx context.Context, id, to string, from ...string) (bool, error) {
	args := []any{to, ts(Now()), id}
	for _, f := range from {
		args = append(args, f)
	}
	res, err := s.exec(ctx, `UPDATE sessions SET status = ?, updated_at = ? WHERE id = ? AND status IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(from)), ",")+`)`, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// HeartbeatSession refreshes last_heartbeat.
func (s *Store) HeartbeatSession(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `UPDATE sessions SET last_heartbeat = ? WHERE id = ?`, ts(Now()), id)
	return err
}

// DeleteSession removes a session row.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return affected(res)
}

// PinnedImage is an image pinned by digest.
type PinnedImage struct {
	Ref      string
	Digest   string
	PinnedAt time.Time
}

// PinImage records the digest for ref.
func (s *Store) PinImage(ctx context.Context, ref, digest string) error {
	_, err := s.exec(ctx, `INSERT INTO images (ref, digest, pinned_at) VALUES (?,?,?)
		ON CONFLICT (ref) DO UPDATE SET digest = excluded.digest, pinned_at = excluded.pinned_at`, ref, digest, ts(Now()))
	return err
}

// GetPinnedImage returns the pinned digest of ref.
func (s *Store) GetPinnedImage(ctx context.Context, ref string) (*PinnedImage, error) {
	var p PinnedImage
	var at string
	err := s.queryRow(ctx, `SELECT ref, digest, pinned_at FROM images WHERE ref = ?`, ref).Scan(&p.Ref, &p.Digest, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.PinnedAt = parseTS(at)
	return &p, nil
}

// ListPinnedImages returns all pinned images.
func (s *Store) ListPinnedImages(ctx context.Context) ([]*PinnedImage, error) {
	rows, err := s.query(ctx, `SELECT ref, digest, pinned_at FROM images ORDER BY ref`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*PinnedImage
	for rows.Next() {
		var p PinnedImage
		var at string
		if err := rows.Scan(&p.Ref, &p.Digest, &at); err != nil {
			return nil, err
		}
		p.PinnedAt = parseTS(at)
		out = append(out, &p)
	}
	return out, rows.Err()
}

// TouchRepository records a fetch of a cached repository.
func (s *Store) TouchRepository(ctx context.Context, remote, path string) error {
	_, err := s.exec(ctx, `INSERT INTO repositories (remote, path, fetched_at) VALUES (?,?,?)
		ON CONFLICT (remote) DO UPDATE SET path = excluded.path, fetched_at = excluded.fetched_at`, remote, path, ts(Now()))
	return err
}

// SessionRepo is a worktree resolved for a run session.
type SessionRepo struct {
	SessionID string
	Ref       string
	Revision  string
	Path      string
}

// GetSessionRepo returns the worktree a session resolved for ref@revision.
func (s *Store) GetSessionRepo(ctx context.Context, sessionID, ref, revision string) (*SessionRepo, error) {
	r := SessionRepo{SessionID: sessionID, Ref: ref, Revision: revision}
	err := s.queryRow(ctx, `SELECT path FROM session_repos WHERE session_id = ? AND ref = ? AND revision = ?`, sessionID, ref, revision).Scan(&r.Path)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// AddSessionRepo records a resolved worktree.
func (s *Store) AddSessionRepo(ctx context.Context, r *SessionRepo) error {
	_, err := s.exec(ctx, `INSERT INTO session_repos (session_id, ref, revision, path) VALUES (?,?,?,?)`, r.SessionID, r.Ref, r.Revision, r.Path)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// ListSessionRepos returns the worktrees resolved by a session.
func (s *Store) ListSessionRepos(ctx context.Context, sessionID string) ([]*SessionRepo, error) {
	rows, err := s.query(ctx, `SELECT session_id, ref, revision, path FROM session_repos WHERE session_id = ? ORDER BY ref, revision`, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*SessionRepo
	for rows.Next() {
		var r SessionRepo
		if err := rows.Scan(&r.SessionID, &r.Ref, &r.Revision, &r.Path); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}
