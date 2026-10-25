-- +goose Up
-- Runs keep pointing at the workflow version they execute, so deleting a
-- workflow must not cascade its versions away: a queued run would fail
-- to load its version and GetRun would lose the steps' needs.
-- DeleteWorkflow removes the versions no run references.
ALTER TABLE workflow_versions DROP CONSTRAINT IF EXISTS workflow_versions_workflow_id_fkey;

-- +goose Down
DELETE FROM workflow_versions WHERE workflow_id NOT IN (SELECT id FROM workflows);
ALTER TABLE workflow_versions ADD CONSTRAINT workflow_versions_workflow_id_fkey
    FOREIGN KEY (workflow_id) REFERENCES workflows(id) ON DELETE CASCADE;
