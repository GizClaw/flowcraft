-- +goose Up
--
-- Split per-model config and the default-model pointer out of
-- provider_configs, where they had been multiplexed via PK prefixes
-- ("model:<provider>/<model>", "__global_default__"). The encoding
-- worked but cost us at every read site: handlers had to dispatch on
-- string prefixes, and the SDK now exposes typed extension interfaces
-- (llm.ModelConfigStore, llm.DefaultModelStore) that the store can
-- satisfy directly only with proper tables.
--
-- After this migration:
--
--   - provider_configs holds ONLY plain provider credential rows.
--   - model_configs holds per-model overrides ({caps, extra}) keyed
--     by (provider, model). Schema is forward-compatible: caps and
--     extra are JSON blobs the SDK already knows how to consume.
--   - default_model is a single-row pointer ({provider, model}).
--     The CHECK(id=1) constraint mirrors owner_credential's pattern
--     for "well-known singleton row".
--
-- Data migration is one-shot and idempotent (INSERT OR IGNORE +
-- post-DELETE), so re-runs on already-migrated databases are no-ops.

CREATE TABLE IF NOT EXISTS model_configs (
    provider TEXT NOT NULL,
    model    TEXT NOT NULL,
    caps     TEXT NOT NULL DEFAULT '{}',
    extra    TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY (provider, model)
);

CREATE TABLE IF NOT EXISTS default_model (
    id       INTEGER PRIMARY KEY CHECK (id = 1),
    provider TEXT NOT NULL,
    model    TEXT NOT NULL
);

-- Move "model:<provider>/<model>" rows.
--
-- The legacy JSON blob may carry per-model "caps" and/or "extra"
-- sub-objects (anything else was historical noise we drop). When
-- neither is present we still want a row to preserve the "this model
-- is configured" signal that ListModels relies on, so we default to
-- empty objects.
INSERT OR IGNORE INTO model_configs (provider, model, caps, extra)
SELECT
    substr(provider, 7, instr(substr(provider, 7), '/') - 1)               AS provider,
    substr(provider, 7 + instr(substr(provider, 7), '/'))                  AS model,
    COALESCE(json_extract(config, '$.caps'),  '{}')                        AS caps,
    COALESCE(json_extract(config, '$.extra'), '{}')                        AS extra
FROM provider_configs
WHERE provider LIKE 'model:%/%'
  AND substr(provider, 7) NOT LIKE '/%'      -- guard "model:/foo"
  AND substr(provider, 7) NOT LIKE '%/';     -- guard "model:foo/"

-- Only remove per-model rows that were successfully migrated above.
-- The GUARD in the INSERT skips malformed keys like "model:no-slash"
-- or "model:foo/" — leaving them here instead of silently dropping
-- preserves the chance for a human to inspect them (they never would
-- have been surfaced by ListModels regardless).
DELETE FROM provider_configs
WHERE provider LIKE 'model:%/%'
  AND substr(provider, 7) NOT LIKE '/%'
  AND substr(provider, 7) NOT LIKE '%/';

-- Move "__global_default__" pointer row.
INSERT OR IGNORE INTO default_model (id, provider, model)
SELECT 1,
       json_extract(config, '$.provider'),
       json_extract(config, '$.model')
FROM provider_configs
WHERE provider = '__global_default__'
  AND json_extract(config, '$.provider') IS NOT NULL
  AND json_extract(config, '$.provider') != ''
  AND json_extract(config, '$.model') IS NOT NULL
  AND json_extract(config, '$.model') != '';

DELETE FROM provider_configs WHERE provider = '__global_default__';

-- +goose Down
--
-- Reverse split: fold model_configs and default_model back into
-- provider_configs using the legacy PK-prefix encoding so older code
-- can read the data.
INSERT OR REPLACE INTO provider_configs (provider, config)
SELECT 'model:' || provider || '/' || model,
       json_object('caps', json(caps), 'extra', json(extra))
FROM model_configs;

INSERT OR REPLACE INTO provider_configs (provider, config)
SELECT '__global_default__',
       json_object('provider', provider, 'model', model)
FROM default_model;

DROP TABLE IF EXISTS default_model;
DROP TABLE IF EXISTS model_configs;
