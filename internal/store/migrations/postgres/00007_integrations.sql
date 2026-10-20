-- +goose Up
-- Phase 3 (ADR 0007): delivery records keyed by integration, per-workflow
-- URL tokens for the generic webhook, and persisted cron state. The
-- legacy per-workflow webhook tables from 00001 are replaced.
DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS schedule_state;

CREATE TABLE webhook_deliveries (
    id            TEXT PRIMARY KEY,
    integration   TEXT NOT NULL,
    delivery_key  TEXT NOT NULL,
    workflow      TEXT NOT NULL DEFAULT '',
    method        TEXT NOT NULL,
    headers_json  TEXT NOT NULL,
    query         TEXT NOT NULL DEFAULT '',
    body          BYTEA NOT NULL,
    verified      INTEGER NOT NULL,
    verify_error  TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL,
    error         TEXT NOT NULL DEFAULT '',
    events_json   TEXT NOT NULL DEFAULT '[]',
    results_json  TEXT NOT NULL DEFAULT '[]',
    redelivery_of TEXT,
    received_at   TEXT NOT NULL
);
CREATE INDEX webhook_deliveries_key ON webhook_deliveries(integration, delivery_key);
CREATE INDEX webhook_deliveries_received ON webhook_deliveries(received_at);

CREATE TABLE webhook_tokens (
    workflow   TEXT PRIMARY KEY,
    token      TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE cron_state (
    workflow      TEXT NOT NULL,
    idx           INTEGER NOT NULL,
    expr          TEXT NOT NULL,
    next_at       TEXT NOT NULL,
    last_fired_at TEXT,
    PRIMARY KEY (workflow, idx)
);

-- +goose Down
DROP TABLE cron_state;
DROP TABLE webhook_tokens;
DROP TABLE webhook_deliveries;
CREATE TABLE webhook_deliveries (
    id           TEXT PRIMARY KEY,
    workflow_id  TEXT,
    provider     TEXT NOT NULL,
    headers_json TEXT NOT NULL,
    body         BYTEA NOT NULL,
    verified     BOOLEAN NOT NULL,
    verify_error TEXT NOT NULL DEFAULT '',
    run_id       TEXT,
    received_at  TEXT NOT NULL
);
CREATE TABLE schedule_state (
    workflow_id   TEXT NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    idx           INTEGER NOT NULL,
    cron          TEXT NOT NULL,
    input_json    TEXT NOT NULL,
    last_fired_at TEXT,
    next_at       TEXT NOT NULL,
    PRIMARY KEY (workflow_id, idx)
);
