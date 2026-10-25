-- +goose Up
-- A browser session signed in with an API token records that token's id,
-- so revoking the token also ends the session. Sessions from one-time
-- login codes (`icaro ui`) have no token and keep token_id NULL.
-- Rows from before this column cannot be tied to a token, so they are
-- dropped: everyone signs in to the web UI once more.
DELETE FROM ui_sessions;
ALTER TABLE ui_sessions ADD COLUMN token_id TEXT;
CREATE INDEX ui_sessions_token ON ui_sessions(token_id);

-- +goose Down
DROP INDEX ui_sessions_token;
ALTER TABLE ui_sessions DROP COLUMN token_id;
