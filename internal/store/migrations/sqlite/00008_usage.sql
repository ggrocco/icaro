-- +goose Up
-- Phase 9: provider usage parsed from local agent logs. One row per
-- (provider, source, day, model); re-collection replaces the row. cost_usd
-- is an estimate from a pricing table and is NULL when no entry exists.
CREATE TABLE usage_snapshots (
    provider           TEXT NOT NULL,
    source             TEXT NOT NULL,
    day                TEXT NOT NULL,
    model              TEXT NOT NULL,
    input_tokens       INTEGER NOT NULL DEFAULT 0,
    output_tokens      INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    messages           INTEGER NOT NULL DEFAULT 0,
    cost_usd           REAL,
    confidence         TEXT NOT NULL,
    collected_at       TEXT NOT NULL,
    PRIMARY KEY (provider, source, day, model)
);
CREATE INDEX usage_snapshots_day ON usage_snapshots(day);

-- +goose Down
DROP TABLE usage_snapshots;
