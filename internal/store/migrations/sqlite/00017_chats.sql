-- +goose Up
-- Workflow assistant chats (ADR 0035). A chat is listed globally; an
-- optional project scopes its memory reads. session_id is the chat's
-- assist session and, like runs.session_id, has no foreign key: the chat
-- outlives a removed session. conversation_id is the agent CLI's own id,
-- used to resume the conversation on the next turn.
CREATE TABLE chats (
    id              TEXT PRIMARY KEY,
    title           TEXT NOT NULL DEFAULT '',
    profile         TEXT NOT NULL,
    agent           TEXT NOT NULL,
    project_id      TEXT NOT NULL DEFAULT '',
    workflow        TEXT NOT NULL DEFAULT '',
    session_id      TEXT NOT NULL DEFAULT '',
    conversation_id TEXT NOT NULL DEFAULT '',
    created_by      TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);
CREATE INDEX chats_updated ON chats (updated_at);
CREATE INDEX chats_session ON chats (session_id);

-- The visible messages, in order. An assistant row is one turn: its
-- status follows the turn (queued, running, done, error, cancelled,
-- interrupted, rate_limited, auth_required). Text is capped; tool calls
-- and the raw stream live in the chat's append-only log from log_offset.
CREATE TABLE chat_messages (
    id            TEXT PRIMARY KEY,
    chat_id       TEXT NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    seq           INTEGER NOT NULL,
    role          TEXT NOT NULL,
    text          TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL,
    error         TEXT NOT NULL DEFAULT '',
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read    INTEGER NOT NULL DEFAULT 0,
    cache_write   INTEGER NOT NULL DEFAULT 0,
    cost_micro    INTEGER NOT NULL DEFAULT 0,
    log_offset    INTEGER NOT NULL DEFAULT 0,
    reset_at      TEXT,
    created_at    TEXT NOT NULL,
    finished_at   TEXT,
    UNIQUE (chat_id, seq)
);
CREATE INDEX chat_messages_status ON chat_messages (status);

-- The chat's draft, shared with the editor: the YAML, the validator's
-- issues (JSON) and who wrote it last. version counts every write.
CREATE TABLE chat_drafts (
    chat_id    TEXT PRIMARY KEY REFERENCES chats(id) ON DELETE CASCADE,
    yaml       TEXT NOT NULL,
    issues     TEXT NOT NULL DEFAULT '[]',
    source     TEXT NOT NULL,
    version    INTEGER NOT NULL,
    updated_at TEXT NOT NULL
);

-- A gated tool call (run_workflow) waiting for a person: what it would
-- do (JSON), the decision (approved, declined, expired), who made it,
-- and its result (the run id).
CREATE TABLE chat_approvals (
    id         TEXT PRIMARY KEY,
    chat_id    TEXT NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    tool       TEXT NOT NULL,
    request    TEXT NOT NULL,
    decision   TEXT NOT NULL DEFAULT '',
    actor      TEXT NOT NULL DEFAULT '',
    result     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    decided_at TEXT
);
CREATE INDEX chat_approvals_chat ON chat_approvals (chat_id, decision);

-- +goose Down
DROP TABLE chat_approvals;
DROP TABLE chat_drafts;
DROP TABLE chat_messages;
DROP TABLE chats;
