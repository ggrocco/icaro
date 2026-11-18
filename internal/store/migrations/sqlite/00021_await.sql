-- +goose Up
-- Await steps (ADR 0032).
--
-- delivery_events holds the normalized payload of each event a verified
-- delivery matched, written before waiting steps are read, so an await
-- step that starts later can look back for an event it missed. workflow
-- is the generic webhook token's workflow ('' otherwise). Rows go with
-- their delivery when it is pruned.
CREATE TABLE delivery_events (
    delivery_id  TEXT NOT NULL REFERENCES webhook_deliveries(id) ON DELETE CASCADE,
    seq          INTEGER NOT NULL,
    integration  TEXT NOT NULL,
    event        TEXT NOT NULL,
    workflow     TEXT NOT NULL DEFAULT '',
    payload_json TEXT NOT NULL,
    received_at  TEXT NOT NULL,
    PRIMARY KEY (delivery_id, seq)
);
CREATE INDEX delivery_events_lookup ON delivery_events (integration, event, received_at);

-- step_awaits holds one row per await step that started: the event it
-- waits for, the rendered key with the step's match and filter (so a
-- delivery is checked without loading workflow specs), the deadline, and how it ended (matched,
-- expired, cancelled) with the delivery (or manual:<id>), the payload and
-- who resolved it. State leaves 'waiting' only by compare-and-set.
CREATE TABLE step_awaits (
    run_id       TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_id      TEXT NOT NULL,
    integration  TEXT NOT NULL,
    event        TEXT NOT NULL,
    workflow     TEXT NOT NULL,
    match_key    TEXT NOT NULL,
    match_expr   TEXT NOT NULL,
    filter_expr  TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL DEFAULT 'waiting',
    deadline     TEXT NOT NULL,
    delivery_id  TEXT NOT NULL DEFAULT '',
    payload_json TEXT NOT NULL DEFAULT '',
    actor        TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    decided_at   TEXT,
    PRIMARY KEY (run_id, step_id)
);
CREATE INDEX step_awaits_waiting ON step_awaits (integration, event, state);

-- +goose Down
DROP TABLE step_awaits;
DROP TABLE delivery_events;
