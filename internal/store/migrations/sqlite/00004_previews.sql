-- +goose Up
-- One row per worktree with a compose file (architecture §7). session_id
-- is the owning session when a session created the worktree.
CREATE TABLE preview_instances (
    id              TEXT PRIMARY KEY,
    session_id      TEXT,
    project_id      TEXT NOT NULL DEFAULT '',
    worktree_path   TEXT NOT NULL UNIQUE,
    compose_file    TEXT NOT NULL DEFAULT '',
    slug            TEXT NOT NULL,
    hostname        TEXT NOT NULL UNIQUE,
    compose_project TEXT NOT NULL UNIQUE,
    service         TEXT NOT NULL DEFAULT '',
    port            INTEGER NOT NULL DEFAULT 0,
    status          TEXT NOT NULL,
    config_hash     TEXT NOT NULL DEFAULT '',
    url             TEXT NOT NULL DEFAULT '',
    error           TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);
CREATE INDEX preview_instances_session ON preview_instances(session_id);

-- Compose-lint findings a user explicitly allowed for one worktree.
CREATE TABLE preview_allowances (
    worktree_path TEXT NOT NULL,
    finding_id    TEXT NOT NULL,
    actor         TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    PRIMARY KEY (worktree_path, finding_id)
);

-- +goose Down
DROP TABLE preview_allowances;
DROP TABLE preview_instances;
