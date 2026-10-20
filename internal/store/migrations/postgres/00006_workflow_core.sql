-- +goose Up
-- Phase 2: DAG step state keyed by step id, indexed logs, idempotent and
-- concurrency-aware runs. The legacy run_steps rows referenced the
-- superseded linear spec and cannot be executed by the DAG engine.
DROP TABLE run_steps;

ALTER TABLE runs ADD COLUMN idempotency_key TEXT;
ALTER TABLE runs ADD COLUMN concurrency TEXT NOT NULL DEFAULT 'allow';
ALTER TABLE runs ADD COLUMN session_id TEXT;
ALTER TABLE runs ADD COLUMN delivery_id TEXT;
ALTER TABLE runs ADD COLUMN actor TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX runs_idempotency_key ON runs(idempotency_key);
CREATE INDEX runs_workflow_status ON runs(workflow_id, status);

CREATE TABLE step_runs (
    run_id       TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_id      TEXT NOT NULL,
    idx          INTEGER NOT NULL,
    kind         TEXT NOT NULL,
    status       TEXT NOT NULL,
    attempt      INTEGER NOT NULL DEFAULT 0,
    session_id   TEXT NOT NULL DEFAULT '',
    container_id TEXT NOT NULL DEFAULT '',
    exit_code    INTEGER,
    outputs_json TEXT NOT NULL DEFAULT '{}',
    log_path     TEXT NOT NULL DEFAULT '',
    log_size     INTEGER NOT NULL DEFAULT 0,
    log_lines    INTEGER NOT NULL DEFAULT 0,
    started_at   TEXT,
    finished_at  TEXT,
    error        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, step_id)
);

-- One row per flushed log segment: the byte offset in the step's log file
-- and the cumulative number of lines written up to it. Tailing starts from
-- any offset; re-attaching resumes capture from the last chunk.
CREATE TABLE log_chunks (
    run_id      TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_id     TEXT NOT NULL,
    attempt     INTEGER NOT NULL,
    seq         INTEGER NOT NULL,
    file_offset INTEGER NOT NULL,
    line_count  INTEGER NOT NULL,
    created_at  TEXT NOT NULL,
    PRIMARY KEY (run_id, step_id, attempt, seq)
);

-- +goose Down
DROP TABLE log_chunks;
DROP TABLE step_runs;
DROP INDEX runs_workflow_status;
DROP INDEX runs_idempotency_key;
ALTER TABLE runs DROP COLUMN actor;
ALTER TABLE runs DROP COLUMN delivery_id;
ALTER TABLE runs DROP COLUMN session_id;
ALTER TABLE runs DROP COLUMN concurrency;
ALTER TABLE runs DROP COLUMN idempotency_key;
CREATE TABLE run_steps (
    run_id       TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    idx          INTEGER NOT NULL,
    name         TEXT NOT NULL,
    status       TEXT NOT NULL,
    attempt      INTEGER NOT NULL DEFAULT 0,
    container_id TEXT NOT NULL DEFAULT '',
    exit_code    INTEGER,
    output_json  TEXT NOT NULL DEFAULT '{}',
    log_path     TEXT NOT NULL DEFAULT '',
    log_size     INTEGER NOT NULL DEFAULT 0,
    started_at   TEXT,
    finished_at  TEXT,
    error        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, idx)
);
