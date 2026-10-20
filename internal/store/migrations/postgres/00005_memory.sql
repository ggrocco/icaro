-- +goose Up
-- Shared memory (architecture §8, ADR 0004, ADR 0020). Scope columns are
-- plain text: project ids come from the session identity, never from the
-- caller's arguments. Full-text search is a portable inverted index
-- (memory_terms) so SQLite and Postgres run the same SQL.
CREATE TABLE memories (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL,
    branch       TEXT NOT NULL DEFAULT '',
    worktree     TEXT NOT NULL DEFAULT '',
    agent        TEXT NOT NULL DEFAULT '',
    session_id   TEXT NOT NULL DEFAULT '',
    origin       TEXT NOT NULL,
    kind         TEXT NOT NULL,
    title        TEXT NOT NULL,
    body         TEXT NOT NULL DEFAULT '',
    source       TEXT NOT NULL,
    source_line  INTEGER NOT NULL DEFAULT 0,
    link         TEXT NOT NULL DEFAULT '',
    content_hash TEXT NOT NULL DEFAULT '',
    doc_len      INTEGER NOT NULL DEFAULT 0,
    status       TEXT NOT NULL DEFAULT '',
    accepted_by  TEXT NOT NULL DEFAULT '',
    accepted_at  TEXT,
    metadata     TEXT NOT NULL DEFAULT '{}',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    expires_at   TEXT
);
CREATE INDEX memories_project_kind ON memories(project_id, kind, created_at);
CREATE INDEX memories_project_source ON memories(project_id, source);
CREATE INDEX memories_expires ON memories(expires_at);
-- Covers the BM25 corpus statistics (count and average length).
CREATE INDEX memories_project_len ON memories(project_id, expires_at, doc_len);

-- One posting per (term, memory). tf is weighted (title terms count
-- double) and doc_len is copied from the memory so ranking reads only the
-- covering index. Postings are replaced per memory, so (term, memory_id)
-- needs no unique index of its own.
CREATE TABLE memory_terms (
    term       TEXT NOT NULL,
    memory_id  TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    project_id TEXT NOT NULL,
    tf         INTEGER NOT NULL,
    doc_len    INTEGER NOT NULL
);
CREATE INDEX memory_terms_lookup ON memory_terms(project_id, term, tf, doc_len, memory_id);
CREATE INDEX memory_terms_memory ON memory_terms(memory_id);

CREATE TABLE memory_edges (
    project_id TEXT NOT NULL,
    from_id    TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    to_ref     TEXT NOT NULL,
    kind       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (from_id, to_ref, kind)
);
CREATE INDEX memory_edges_project_to ON memory_edges(project_id, to_ref);

-- +goose Down
DROP TABLE memory_edges;
DROP TABLE memory_terms;
DROP TABLE memories;
