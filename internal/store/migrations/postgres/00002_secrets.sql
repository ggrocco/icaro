-- +goose Up
-- Named secrets for ${{ secrets.* }} and host-bound integration
-- credentials. Values are sealed with the master key; hosts is a JSON
-- array of hostnames the value may be sent to (empty = not bound).
CREATE TABLE secrets (
    name       TEXT PRIMARY KEY,
    value_ct   BYTEA NOT NULL,
    hosts_json TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE secrets;
