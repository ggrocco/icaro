-- +goose Up
-- The memory distiller's queue (ADR 0036): one row per chat, claimed with
-- a compare-and-set on status. stage2 records what the model stage did
-- (done, skipped: <why>, failed: <why>); run_at is when a retry is due.
CREATE TABLE distill_jobs (
    chat_id    TEXT PRIMARY KEY,
    status     TEXT NOT NULL,
    attempts   INTEGER NOT NULL DEFAULT 0,
    error      TEXT NOT NULL DEFAULT '',
    stage2     TEXT NOT NULL DEFAULT '',
    memories   INTEGER NOT NULL DEFAULT 0,
    run_at     TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX distill_jobs_due ON distill_jobs (status, run_at);

-- Per-chat convergence metrics, written by the distiller, so chats that
-- started with memory can be compared with chats that did not. -1 means
-- "never happened".
CREATE TABLE chat_stats (
    chat_id         TEXT PRIMARY KEY,
    used_memories   INTEGER NOT NULL DEFAULT 0,
    turns           INTEGER NOT NULL DEFAULT 0,
    validations     INTEGER NOT NULL DEFAULT -1,
    proposals       INTEGER NOT NULL DEFAULT 0,
    saves           INTEGER NOT NULL DEFAULT 0,
    turns_to_save   INTEGER NOT NULL DEFAULT -1,
    seconds_to_save INTEGER NOT NULL DEFAULT -1,
    updated_at      TEXT NOT NULL
);

-- Memories a person removed from a chat's distilled memories, so the
-- next run does not write them again.
CREATE TABLE distill_removed (
    chat_id    TEXT NOT NULL,
    memory_id  TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (chat_id, memory_id)
);

-- +goose Down
DROP TABLE distill_removed;
DROP TABLE chat_stats;
DROP TABLE distill_jobs;
