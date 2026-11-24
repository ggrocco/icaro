-- +goose Up
-- Agent steps (ADR 0037): the profile an agent step ran under, the image
-- digest pinned for its run, and the token usage its stream reported.
ALTER TABLE step_runs ADD COLUMN profile TEXT NOT NULL DEFAULT '';
ALTER TABLE step_runs ADD COLUMN image_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE step_runs ADD COLUMN input_tokens BIGINT NOT NULL DEFAULT 0;
ALTER TABLE step_runs ADD COLUMN output_tokens BIGINT NOT NULL DEFAULT 0;
ALTER TABLE step_runs ADD COLUMN cache_read BIGINT NOT NULL DEFAULT 0;
ALTER TABLE step_runs ADD COLUMN cache_write BIGINT NOT NULL DEFAULT 0;
ALTER TABLE step_runs ADD COLUMN cost_micro BIGINT NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE step_runs DROP COLUMN cost_micro;
ALTER TABLE step_runs DROP COLUMN cache_write;
ALTER TABLE step_runs DROP COLUMN cache_read;
ALTER TABLE step_runs DROP COLUMN output_tokens;
ALTER TABLE step_runs DROP COLUMN input_tokens;
ALTER TABLE step_runs DROP COLUMN image_digest;
ALTER TABLE step_runs DROP COLUMN profile;
