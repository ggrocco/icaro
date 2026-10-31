-- +goose Up
-- Approval and wait steps (ADR 0030). A run whose remaining work is only
-- such steps is parked as "waiting" and gives its engine slot back;
-- wake_at is when the engine next looks at it (a wait ending, an approval
-- deadline, or now after a decision or a cancel request). NULL waits for a
-- decision only.
ALTER TABLE runs ADD COLUMN wake_at TEXT;
CREATE INDEX runs_status_wake ON runs (status, wake_at);

-- One row per approval step that has started waiting: the message the
-- approver saw, and the decision (approved, rejected, expired), who made
-- it and why.
CREATE TABLE step_approvals (
    run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_id    TEXT NOT NULL,
    message    TEXT NOT NULL,
    decision   TEXT NOT NULL DEFAULT '',
    actor      TEXT NOT NULL DEFAULT '',
    comment    TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    decided_at TEXT,
    PRIMARY KEY (run_id, step_id)
);

-- +goose Down
DROP TABLE step_approvals;
DROP INDEX runs_status_wake;
ALTER TABLE runs DROP COLUMN wake_at;
