package store

import (
	"context"
	"database/sql"
	"errors"
)

const workflowCols = `id, name, version, status, spec_yaml, spec_json, webhook_token, webhook_secret_ct, created_at, updated_at`

func scanWorkflow(sc interface{ Scan(...any) error }) (*Workflow, error) {
	var w Workflow
	var token sql.NullString
	var created, updated string
	if err := sc.Scan(&w.ID, &w.Name, &w.Version, &w.Status, &w.SpecYAML, &w.SpecJSON, &token, &w.WebhookSecretCT, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	w.WebhookToken = token.String
	w.CreatedAt, w.UpdatedAt = parseTS(created), parseTS(updated)
	return &w, nil
}

// ApplyWorkflow inserts a workflow or bumps its version, recording the
// version row. It returns the stored workflow. Name is the natural key.
func (s *Store) ApplyWorkflow(ctx context.Context, name, specYAML, specJSON, actor string) (*Workflow, error) {
	now := ts(Now())
	var out *Workflow
	err := s.tx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, s.rebind(`SELECT `+workflowCols+` FROM workflows WHERE name = ?`), name)
		existing, err := scanWorkflow(row)
		switch {
		case errors.Is(err, ErrNotFound):
			w := &Workflow{ID: NewID(), Name: name, Version: 1, Status: WorkflowActive, SpecYAML: specYAML, SpecJSON: specJSON}
			if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO workflows (`+workflowCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)`),
				w.ID, w.Name, w.Version, w.Status, w.SpecYAML, w.SpecJSON, nil, nil, now, now); err != nil {
				return err
			}
			w.CreatedAt, w.UpdatedAt = parseTS(now), parseTS(now)
			out = w
		case err != nil:
			return err
		default:
			existing.Version++
			existing.SpecYAML, existing.SpecJSON = specYAML, specJSON
			if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE workflows SET version = ?, spec_yaml = ?, spec_json = ?, updated_at = ? WHERE id = ?`),
				existing.Version, specYAML, specJSON, now, existing.ID); err != nil {
				return err
			}
			existing.UpdatedAt = parseTS(now)
			out = existing
		}
		_, err = tx.ExecContext(ctx, s.rebind(`INSERT INTO workflow_versions (workflow_id, version, spec_yaml, actor, created_at) VALUES (?,?,?,?,?)`),
			out.ID, out.Version, specYAML, actor, now)
		return err
	})
	return out, err
}

// GetWorkflow fetches by name.
func (s *Store) GetWorkflow(ctx context.Context, name string) (*Workflow, error) {
	return scanWorkflow(s.queryRow(ctx, `SELECT `+workflowCols+` FROM workflows WHERE name = ?`, name))
}

// GetWorkflowByID fetches by id.
func (s *Store) GetWorkflowByID(ctx context.Context, id string) (*Workflow, error) {
	return scanWorkflow(s.queryRow(ctx, `SELECT `+workflowCols+` FROM workflows WHERE id = ?`, id))
}

// ListWorkflows returns all workflows ordered by name.
func (s *Store) ListWorkflows(ctx context.Context) ([]*Workflow, error) {
	rows, err := s.query(ctx, `SELECT `+workflowCols+` FROM workflows ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Workflow
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SetWorkflowStatus toggles active/disabled/draft.
func (s *Store) SetWorkflowStatus(ctx context.Context, id, status string) error {
	res, err := s.exec(ctx, `UPDATE workflows SET status = ?, updated_at = ? WHERE id = ?`, status, ts(Now()), id)
	if err != nil {
		return err
	}
	return affected(res)
}

// SetWebhook stores the webhook token and (optional) encrypted secret.
func (s *Store) SetWebhook(ctx context.Context, id, token string, secretCT []byte) error {
	res, err := s.exec(ctx, `UPDATE workflows SET webhook_token = ?, webhook_secret_ct = ?, updated_at = ? WHERE id = ?`, nullStr(token), secretCT, ts(Now()), id)
	if err != nil {
		return err
	}
	return affected(res)
}

// DeleteWorkflow removes a workflow and its versions (runs are kept).
func (s *Store) DeleteWorkflow(ctx context.Context, id string) error {
	res, err := s.exec(ctx, `DELETE FROM workflows WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return affected(res)
}

// ListWorkflowVersions returns versions newest first.
func (s *Store) ListWorkflowVersions(ctx context.Context, workflowID string) ([]*WorkflowVersion, error) {
	rows, err := s.query(ctx, `SELECT workflow_id, version, spec_yaml, actor, created_at FROM workflow_versions WHERE workflow_id = ? ORDER BY version DESC`, workflowID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*WorkflowVersion
	for rows.Next() {
		var v WorkflowVersion
		var created string
		if err := rows.Scan(&v.WorkflowID, &v.Version, &v.SpecYAML, &v.Actor, &created); err != nil {
			return nil, err
		}
		v.CreatedAt = parseTS(created)
		out = append(out, &v)
	}
	return out, rows.Err()
}

// GetWorkflowVersion fetches one historical version.
func (s *Store) GetWorkflowVersion(ctx context.Context, workflowID string, version int) (*WorkflowVersion, error) {
	var v WorkflowVersion
	var created string
	err := s.queryRow(ctx, `SELECT workflow_id, version, spec_yaml, actor, created_at FROM workflow_versions WHERE workflow_id = ? AND version = ?`, workflowID, version).
		Scan(&v.WorkflowID, &v.Version, &v.SpecYAML, &v.Actor, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v.CreatedAt = parseTS(created)
	return &v, nil
}

func affected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
