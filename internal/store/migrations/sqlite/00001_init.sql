-- +goose Up
CREATE TABLE workflows (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL UNIQUE,
    version           INTEGER NOT NULL,
    status            TEXT NOT NULL,
    spec_yaml         TEXT NOT NULL,
    spec_json         TEXT NOT NULL,
    webhook_token     TEXT,
    webhook_secret_ct BLOB,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
);

CREATE TABLE workflow_versions (
    workflow_id TEXT NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    version     INTEGER NOT NULL,
    spec_yaml   TEXT NOT NULL,
    actor       TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    PRIMARY KEY (workflow_id, version)
);

CREATE TABLE runs (
    id                  TEXT PRIMARY KEY,
    workflow_id         TEXT NOT NULL,
    workflow_name       TEXT NOT NULL,
    workflow_version    INTEGER NOT NULL,
    status              TEXT NOT NULL,
    trigger_kind        TEXT NOT NULL,
    input_json          TEXT NOT NULL,
    root_run_id         TEXT NOT NULL,
    parent_run_id       TEXT,
    depth               INTEGER NOT NULL DEFAULT 0,
    rerun_of            TEXT,
    library_hash        TEXT,
    claimed_by          TEXT,
    claimed_at          TEXT,
    heartbeat_at        TEXT,
    started_at          TEXT,
    finished_at         TEXT,
    cancel_requested_at TEXT,
    error               TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL
);
CREATE INDEX runs_status_created ON runs(status, created_at);
CREATE INDEX runs_workflow_created ON runs(workflow_id, created_at);
CREATE INDEX runs_claimed_by ON runs(claimed_by, status);

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

CREATE TABLE connections (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    type        TEXT NOT NULL,
    fields_ct   BLOB NOT NULL,
    field_names TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE TABLE api_tokens (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    token_hash   TEXT NOT NULL UNIQUE,
    scope        TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    last_used_at TEXT,
    revoked_at   TEXT
);

CREATE TABLE webhook_deliveries (
    id           TEXT PRIMARY KEY,
    workflow_id  TEXT,
    provider     TEXT NOT NULL,
    headers_json TEXT NOT NULL,
    body         BLOB NOT NULL,
    verified     INTEGER NOT NULL,
    verify_error TEXT NOT NULL DEFAULT '',
    run_id       TEXT,
    received_at  TEXT NOT NULL
);
CREATE INDEX deliveries_workflow_received ON webhook_deliveries(workflow_id, received_at);

CREATE TABLE schedule_state (
    workflow_id   TEXT NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    idx           INTEGER NOT NULL,
    cron          TEXT NOT NULL,
    input_json    TEXT NOT NULL,
    last_fired_at TEXT,
    next_at       TEXT NOT NULL,
    PRIMARY KEY (workflow_id, idx)
);

CREATE TABLE runner_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- +goose Down
DROP TABLE runner_meta;
DROP TABLE schedule_state;
DROP TABLE webhook_deliveries;
DROP TABLE api_tokens;
DROP TABLE connections;
DROP TABLE run_steps;
DROP TABLE runs;
DROP TABLE workflow_versions;
DROP TABLE workflows;
