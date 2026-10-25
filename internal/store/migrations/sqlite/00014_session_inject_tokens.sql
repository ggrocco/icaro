-- +goose Up
-- Credential injection (ADR 0028): the SHA-256 of the capability token a
-- running agent session presents to the daemon's provider injector. The
-- token itself is never stored; stopping a session deletes its row, and
-- removing the session cascades.
CREATE TABLE session_inject_tokens (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL,
    created_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE session_inject_tokens;
