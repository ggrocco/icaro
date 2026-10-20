package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Preview statuses.
const (
	PreviewPending = "pending"
	PreviewRunning = "running"
	PreviewBlocked = "blocked" // compose lint refused it
	PreviewError   = "error"
	PreviewStopped = "stopped"
)

// PreviewInstance is one worktree's preview.
type PreviewInstance struct {
	ID             string    `json:"id"`
	SessionID      string    `json:"session_id,omitempty"`
	ProjectID      string    `json:"project_id,omitempty"`
	WorktreePath   string    `json:"worktree_path"`
	ComposeFile    string    `json:"compose_file"`
	Slug           string    `json:"slug"`
	Hostname       string    `json:"hostname"`
	ComposeProject string    `json:"compose_project"`
	Service        string    `json:"service,omitempty"`
	Port           int       `json:"port,omitempty"`
	Status         string    `json:"status"`
	ConfigHash     string    `json:"config_hash,omitempty"`
	URL            string    `json:"url,omitempty"`
	Error          string    `json:"error,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

const previewCols = `id, session_id, project_id, worktree_path, compose_file, slug, hostname, compose_project, service, port, status, config_hash, url, error, created_at, updated_at`

func scanPreview(sc interface{ Scan(...any) error }) (*PreviewInstance, error) {
	var p PreviewInstance
	var session sql.NullString
	var created, updated string
	err := sc.Scan(&p.ID, &session, &p.ProjectID, &p.WorktreePath, &p.ComposeFile, &p.Slug, &p.Hostname, &p.ComposeProject,
		&p.Service, &p.Port, &p.Status, &p.ConfigHash, &p.URL, &p.Error, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.SessionID = session.String
	p.CreatedAt, p.UpdatedAt = parseTS(created), parseTS(updated)
	return &p, nil
}

// CreatePreview inserts an instance; ID and timestamps are filled in.
func (s *Store) CreatePreview(ctx context.Context, p *PreviewInstance) error {
	if p.ID == "" {
		p.ID = NewID()
	}
	now := Now()
	p.CreatedAt, p.UpdatedAt = now, now
	if p.Status == "" {
		p.Status = PreviewPending
	}
	_, err := s.exec(ctx, `INSERT INTO preview_instances (`+previewCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, nullStr(p.SessionID), p.ProjectID, p.WorktreePath, p.ComposeFile, p.Slug, p.Hostname, p.ComposeProject,
		p.Service, p.Port, p.Status, p.ConfigHash, p.URL, p.Error, ts(now), ts(now))
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// UpdatePreview writes the mutable fields of an instance.
func (s *Store) UpdatePreview(ctx context.Context, p *PreviewInstance) error {
	p.UpdatedAt = Now()
	res, err := s.exec(ctx, `UPDATE preview_instances SET session_id = ?, project_id = ?, compose_file = ?, service = ?, port = ?, status = ?,
		config_hash = ?, url = ?, error = ?, updated_at = ? WHERE id = ?`,
		nullStr(p.SessionID), p.ProjectID, p.ComposeFile, p.Service, p.Port, p.Status, p.ConfigHash, p.URL, p.Error, ts(p.UpdatedAt), p.ID)
	if err != nil {
		return err
	}
	return affected(res)
}

// GetPreview fetches by id.
func (s *Store) GetPreview(ctx context.Context, id string) (*PreviewInstance, error) {
	return scanPreview(s.queryRow(ctx, `SELECT `+previewCols+` FROM preview_instances WHERE id = ?`, id))
}

// GetPreviewByPath fetches by worktree path.
func (s *Store) GetPreviewByPath(ctx context.Context, path string) (*PreviewInstance, error) {
	return scanPreview(s.queryRow(ctx, `SELECT `+previewCols+` FROM preview_instances WHERE worktree_path = ?`, path))
}

// ListPreviews returns all instances ordered by worktree path.
func (s *Store) ListPreviews(ctx context.Context) ([]*PreviewInstance, error) {
	rows, err := s.query(ctx, `SELECT `+previewCols+` FROM preview_instances ORDER BY worktree_path`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*PreviewInstance
	for rows.Next() {
		p, err := scanPreview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePreview removes an instance; a missing one is not an error.
func (s *Store) DeletePreview(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM preview_instances WHERE id = ?`, id)
	return err
}

// AllowPreviewFinding records a user's explicit override of one lint
// finding for one worktree.
func (s *Store) AllowPreviewFinding(ctx context.Context, path, findingID, actor string) error {
	_, err := s.exec(ctx, `INSERT INTO preview_allowances (worktree_path, finding_id, actor, created_at) VALUES (?,?,?,?)
		ON CONFLICT (worktree_path, finding_id) DO NOTHING`, path, findingID, actor, ts(Now()))
	return err
}

// PreviewAllowances returns the allowed finding IDs for a worktree.
func (s *Store) PreviewAllowances(ctx context.Context, path string) (map[string]bool, error) {
	rows, err := s.query(ctx, `SELECT finding_id FROM preview_allowances WHERE worktree_path = ?`, path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ClearPreviewAllowances drops all overrides for a worktree (on cleanup).
func (s *Store) ClearPreviewAllowances(ctx context.Context, path string) error {
	_, err := s.exec(ctx, `DELETE FROM preview_allowances WHERE worktree_path = ?`, path)
	return err
}
