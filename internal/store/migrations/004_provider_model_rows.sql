-- +goose Up
--
-- Repair provider_configs rows written by the buggy AddModel handler
-- that conflated per-model entries with provider credentials. Pre-fix
-- behavior wrote each "added model" to provider_configs with the
-- provider name as the PK and stuffed `cfg.model = "<model>"` into the
-- JSON blob — overwriting any earlier model and shadowing the real
-- provider credentials. Post-fix the handler writes per-model rows
-- under the "model:<provider>/<model>" key prefix and keeps provider
-- credentials in the bare "<provider>" row.
--
-- This migration is idempotent: it scans existing rows and, for every
-- non-default, non-prefixed row whose JSON contains a `model` field,
-- materializes the missing "model:" row before stripping the field
-- from the credentials row. Rows that already follow the new schema
-- (no `model` field, or already prefixed) are left untouched.
--
-- We use INSERT OR IGNORE so re-runs are no-ops and so manually-added
-- "model:" rows are not clobbered.

INSERT OR IGNORE INTO provider_configs (provider, config)
SELECT
    'model:' || provider || '/' || json_extract(config, '$.model'),
    '{}'
FROM provider_configs
WHERE provider != '__global_default__'
  AND provider NOT LIKE 'model:%'
  AND json_extract(config, '$.model') IS NOT NULL
  AND json_extract(config, '$.model') != '';

UPDATE provider_configs
SET config = json_remove(config, '$.model')
WHERE provider != '__global_default__'
  AND provider NOT LIKE 'model:%'
  AND json_extract(config, '$.model') IS NOT NULL;

-- +goose Down
--
-- This migration only repairs data; there is no schema change to
-- revert and the original (broken) `model` field cannot be safely
-- restored without losing the per-model rows. Down is a no-op.
SELECT 1;
