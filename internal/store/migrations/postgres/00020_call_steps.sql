-- +goose Up
-- Call steps (ADR 0038): the child run a call step started and the
-- child's workflow version, a run's workflow outputs, and indexes for
-- walking run trees through the lineage columns.
ALTER TABLE step_runs ADD COLUMN child_run_id TEXT NOT NULL DEFAULT '';
ALTER TABLE step_runs ADD COLUMN child_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN outputs_json TEXT NOT NULL DEFAULT '{}';
CREATE INDEX runs_parent ON runs(parent_run_id);
CREATE INDEX runs_root ON runs(root_run_id);

-- +goose Down
DROP INDEX runs_root;
DROP INDEX runs_parent;
ALTER TABLE runs DROP COLUMN outputs_json;
ALTER TABLE step_runs DROP COLUMN child_version;
ALTER TABLE step_runs DROP COLUMN child_run_id;
