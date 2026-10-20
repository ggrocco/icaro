-- +goose Up
-- Browser sessions and one-time login codes for the web UI (ADR 0023).
-- id is the SHA-256 of the cookie value or code; the plaintext is never
-- stored.
CREATE TABLE ui_sessions (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    name       TEXT NOT NULL,
    scope      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE INDEX ui_sessions_expires ON ui_sessions(expires_at);

-- +goose Down
DROP TABLE ui_sessions;
