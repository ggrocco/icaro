-- +goose Up
-- Remote Ícaro daemons this daemon is signed in to (ADR 0033). alias is
-- the short name references use (alias:workflow), label the display
-- name. The API token is sealed with the master key and is only ever
-- sent to url; principal and scope are what the remote reported at
-- sign-in.
CREATE TABLE remote_hosts (
    alias      TEXT PRIMARY KEY,
    label      TEXT NOT NULL,
    url        TEXT NOT NULL,
    token_ct   BLOB NOT NULL,
    principal  TEXT NOT NULL DEFAULT '',
    scope      TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE remote_hosts;
