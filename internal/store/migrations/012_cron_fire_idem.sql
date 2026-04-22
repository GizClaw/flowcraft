-- +goose Up

-- cron_fire_idem: ensures each cron firing is processed exactly once across restarts.
-- fire_key = sha256(rule_id || scheduled_for_unix)[:16]
-- Retained for 30 days; cleanup is done by the retention goroutine.
CREATE TABLE IF NOT EXISTS cron_fire_idem (
    fire_key       TEXT    PRIMARY KEY,
    rule_id        TEXT    NOT NULL,
    scheduled_for  TEXT    NOT NULL,
    ts             TEXT    NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_cron_fire_idem_rule_sch
    ON cron_fire_idem(rule_id, scheduled_for);
CREATE INDEX IF NOT EXISTS idx_cron_fire_idem_ts
    ON cron_fire_idem(ts);

-- +goose Down
DROP TABLE IF EXISTS cron_fire_idem;
