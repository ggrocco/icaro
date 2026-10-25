-- +goose Up
-- Runs keep pointing at the workflow version they execute, so deleting a
-- workflow must not cascade its versions away: a queued run would fail
-- to load its version and GetRun would lose the steps' needs. SQLite
-- cannot drop a foreign key in place, so the table is rebuilt without
-- it; DeleteWorkflow removes the versions no run references.
CREATE TABLE workflow_versions_new (
    workflow_id TEXT NOT NULL,
    version     INTEGER NOT NULL,
    spec_yaml   TEXT NOT NULL,
    actor       TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    PRIMARY KEY (workflow_id, version)
);
INSERT INTO workflow_versions_new (workflow_id, version, spec_yaml, actor, created_at)
    SELECT workflow_id, version, spec_yaml, actor, created_at FROM workflow_versions;
DROP TABLE workflow_versions;
ALTER TABLE workflow_versions_new RENAME TO workflow_versions;

-- +goose Down
DELETE FROM workflow_versions WHERE workflow_id NOT IN (SELECT id FROM workflows);
CREATE TABLE workflow_versions_old (
    workflow_id TEXT NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    version     INTEGER NOT NULL,
    spec_yaml   TEXT NOT NULL,
    actor       TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    PRIMARY KEY (workflow_id, version)
);
INSERT INTO workflow_versions_old (workflow_id, version, spec_yaml, actor, created_at)
    SELECT workflow_id, version, spec_yaml, actor, created_at FROM workflow_versions;
DROP TABLE workflow_versions;
ALTER TABLE workflow_versions_old RENAME TO workflow_versions;
