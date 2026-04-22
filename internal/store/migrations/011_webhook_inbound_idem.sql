-- +goose Up

-- webhook_inbound_idem: inbound webhook idempotency table.
-- Deduplicates inbound webhook deliveries based on X-Idempotency-Key header.
CREATE TABLE IF NOT EXISTS webhook_inbound_idem (
    endpoint_id  TEXT NOT NULL,
    idem_key     TEXT NOT NULL,
    received_id  TEXT NOT NULL,
    seq          INTEGER NOT NULL,
    ts           TEXT NOT NULL,
    PRIMARY KEY (endpoint_id, idem_key)
) STRICT;
CREATE INDEX IF NOT EXISTS idx_webhook_inbound_idem_ts ON webhook_inbound_idem(ts);

-- +goose Down
DROP TABLE IF EXISTS webhook_inbound_idem;
