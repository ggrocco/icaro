-- +goose Up
-- Webhook delivery hardening (03-security). body_size and body_sha256
-- identify a request whose stored body is only a prefix (rejected
-- deliveries keep at most 4 KiB). signed_sha256 is the SHA-256 of the
-- HMAC-signed material, so a replay that only changes an unsigned header
-- (the delivery id) is still recognized as a duplicate.
ALTER TABLE webhook_deliveries ADD COLUMN body_size INTEGER NOT NULL DEFAULT 0;
ALTER TABLE webhook_deliveries ADD COLUMN body_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE webhook_deliveries ADD COLUMN signed_sha256 TEXT NOT NULL DEFAULT '';
UPDATE webhook_deliveries SET body_size = length(body);
CREATE INDEX webhook_deliveries_signed ON webhook_deliveries(integration, signed_sha256);
CREATE INDEX webhook_deliveries_verified_received ON webhook_deliveries(verified, received_at);

-- +goose Down
DROP INDEX webhook_deliveries_verified_received;
DROP INDEX webhook_deliveries_signed;
ALTER TABLE webhook_deliveries DROP COLUMN signed_sha256;
ALTER TABLE webhook_deliveries DROP COLUMN body_sha256;
ALTER TABLE webhook_deliveries DROP COLUMN body_size;
