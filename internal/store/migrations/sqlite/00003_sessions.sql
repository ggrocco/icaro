-- +goose Up
-- A project is a git repository on the host; sessions work on worktrees of it.
CREATE TABLE projects (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    root_path  TEXT NOT NULL UNIQUE,
    remote     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

-- The session is the parent row of the spine (ADR 0006). Run sessions
-- (ADR 0010) have kind = 'run' and carry run_id; their project_id may be ''.
CREATE TABLE sessions (
    id                  TEXT PRIMARY KEY,
    kind                TEXT NOT NULL,
    project_id          TEXT NOT NULL DEFAULT '',
    profile             TEXT NOT NULL,
    branch              TEXT NOT NULL DEFAULT '',
    worktree_path       TEXT NOT NULL DEFAULT '',
    container_id        TEXT NOT NULL DEFAULT '',
    image_digest        TEXT NOT NULL DEFAULT '',
    preview_instance_id TEXT NOT NULL DEFAULT '',
    run_id              TEXT NOT NULL DEFAULT '',
    status              TEXT NOT NULL,
    error               TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    last_heartbeat      TEXT
);
CREATE INDEX sessions_status ON sessions(status, created_at);
CREATE INDEX sessions_project ON sessions(project_id, created_at);
CREATE INDEX sessions_run ON sessions(run_id);

-- Images pinned by digest at setup; doctor re-verifies them.
CREATE TABLE images (
    ref       TEXT PRIMARY KEY,
    digest    TEXT NOT NULL,
    pinned_at TEXT NOT NULL
);

-- Repository cache entries (bare clones keyed by remote).
CREATE TABLE repositories (
    remote     TEXT PRIMARY KEY,
    path       TEXT NOT NULL,
    fetched_at TEXT NOT NULL
);

-- Worktrees a run session resolved, once per (ref, revision).
CREATE TABLE session_repos (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    ref        TEXT NOT NULL,
    revision   TEXT NOT NULL,
    path       TEXT NOT NULL,
    PRIMARY KEY (session_id, ref, revision)
);

-- +goose Down
DROP TABLE session_repos;
DROP TABLE repositories;
DROP TABLE images;
DROP TABLE sessions;
DROP TABLE projects;
