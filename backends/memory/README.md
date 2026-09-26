# FlowCraft Memory Backend

`backends/memory` is the first-party implementation of the `core/memory`
capability contracts. It owns canonical storage, retrieval, and background
maintenance; `core/memory` stays implementation-neutral.

## Implemented scope

- Canonical conversation storage: every committed turn is an immutable,
  idempotent batch in an append-only Log.
- Canonical document storage: every document revision is an immutable Log
  event with a latest-revision pointer in KV.
- Derivation: a policy-scoped worker scans commits after a durable
  watermark, extracts facts (chat line), publishes them to the fact view,
  and feeds every projection lane exactly once per commit.
- Documents: canonical revisions are scanned through their scope-wide outbox
  cursor, chunked by the deterministic knowledge line, published as a
  document hierarchy, and reconciled into the projection lanes.
- Summaries: derived facts compact into immutable L0-L3 summary records with
  an active manifest served as its own retrieval lane.
- Diagnostics: `Assembly.Diagnostics` reports per-scope derivation cursors,
  pending work, and cumulative worker counters; context calls feed
  `memory.context.requests` / `memory.context.items`
  OpenTelemetry counters when the host initializes telemetry.
- Retrieval: the context path fuses the recent lane with the configured
  projection lanes (BM25, entity, and vector when an embed model is
  configured), hydrates candidates, and packs them within the request
  budget.

Lifecycle maintenance is a later phase behind the same assembly SPI.

## Deploy

```yaml
resources:
  ws:
    kind: workspace.Workspace
    impl: local
    settings: {root: ./workspace}
  memories:
    kind: memory.Assembly
    impl: flowcraft
    deps:
      workspace: ws
    settings: {file: ./memory.yaml}
```

`memory.yaml` is strict JSON/YAML with no version field:

```yaml
storage:
  log: {driver: workspace}
  kv: {driver: workspace}
generate: {provider: deepseek, name: deepseek-v4-flash}
embed: {provider: openai, name: text-embedding-3-small}
scopes:
  - {runtime_id: memories, user_id: user-1}
recent:
  max_items: 20
  max_tokens: 2048
fact:
  strategy: simple
  tail_max_chars: 15000
  max_facts: 64
summary:
  chunk_size: 10
  condense_threshold: 6
  group_size: 3
  max_depth: 4
chunk:
  max_runes: 1600
  overlap_runes: 160
lanes:
  fusion: rrf
retrieval:
  rerank: false
interval: 1m
```

`generate` and `embed` are optional: without them the assembly serves the
recent and lexical/entity lanes. `interval: "0"` disables the background
runner while `RunOnce` stays available. Decay, consolidation, and forgetting
are host-owned maintenance jobs: the module keeps the canonical log and
derived views correct, and hosts schedule whatever hygiene they need.

The agent side binds the assembly through the `core/memory/hook` factories
(`memory.context` under `hook.prepare`, `memory.turn` under `hook.commit`).
Turn commits are idempotent per `IdempotencyKey`: retries must reuse the key
of the original turn. Do not re-commit historical transcript under a new key
— it is appended as new messages. The agent hooks use the run id as that key.

## Storage layout

`storage.log` and `storage.kv` select their driver independently:

- `workspace` (default) keeps every structure under the bound workspace.
- `sqlite` uses real transactions with `settings: {path: ./memory.db}`;
  log and KV sharing one path share one connection pool. SQLite stores are
  closed by `Assembly.Close`.
- `postgres` uses pgx with real transactions and a per-stream advisory lock,
  with `settings: {dsn: "postgres://...", schema: flowcraft_memory}`; log and
  KV sharing one DSN+schema share one pool. Live integration tests run when
  `FC_PG_DSN` is set and skip otherwise (CI sets it in the
  `test-memory-postgres` lane).

With the workspace driver all state lives under the bound workspace:

- `storage/v1/log/...` — append-only streams with commit markers and crash
  recovery.
- `storage/v1/kv/...` — current values, the scope catalog, and document
  latest-revision pointers.
- `views/...` — derived fact and summary views.
- `projections/...` — rebuildable BM25 / entity / vector lane snapshots.
- `worker/v1/watermarks/...` — policy-scoped derivation cursors.

## Import safety

`sources/importer` fetches external documents under an explicit policy
(scheme and host allowlists, body-size cap, loopback/private-address
blocking at dial time). Importing stays outside the capability SPI: hosts
fetch through the importer and hand normalized content to `PutDocument`.

## Integrity and evaluation

- `Assembly.Verify(ctx, scope, conversationID)` runs the repair evidence
  checks read-only: dangling fact links, source digest drift, summary
  digests, and projection digest drift.

## Maintenance (host-invoked)

`Assembly.Maintain(ctx, scope)` runs one soft-merge and decay pass: it detects
superseded facts (same entity plus similar text; the newer fact wins) and aged
facts (exponential decay by event time), writes a per-scope read-path
overlay, and returns the plan. Canonical fact text is never edited — the
overlay only multiplies retrieval scores, and the strongest of the supersede
and decay factors wins. The module ships no runner: hosts schedule the call
however they want (cron, queue, or an admin endpoint). Tuning lives in
`maintain.Config` (similarity threshold, supersede factor, decay half-life,
decay floor).
- `eval.Run` drives a scenario (turns + questions) through the capability
  SPI and reports hit rate and latency. The harness is dataset-agnostic;
  LoCoMo/LongMemEval exports convert to scenarios host-side.
  `LoadLoCoMo` / `LoadLongMemEval` convert the upstream JSON (session
  ordering, role mapping, image annotations), `RunAll` executes a dataset,
  and `Baseline` records/compares hit rates for regression gates. The runnable
  CLI and the live-provider deploy wiring live in the sibling `eval` module
  (`backends/memory/eval/cmd/memory-eval`); this library module keeps no
  provider-driver dependency.

Every name segment is encoded before it reaches a filesystem path, so user
input never becomes a path verbatim.

## Context budgets

Context size is bounded by hard ceilings in addition to the configured
values: at most 500 context items, 65536 estimated tokens, and 1 Mi runes per
request, with 500 items / 65536 tokens for the recent lane. Configuration or
request values above those ceilings are rejected (config) or clamped
(requests). Per-item ceilings keep a single item below the 10k-token limit:
`chunk.max_runes ≤ 8000`, `chunk.summary.max_runes ≤ 4000`,
`fact.max_fact_chars ≤ 8000`, `fact.max_embedding_input_chars ≤ 64000`,
`fact.tail_max_chars ≤ 200000`, `fact.max_facts ≤ 512`.

Token counts are estimates, not tokenizer output: ASCII counts as a quarter
token per rune, other non-ASCII as half a token, and CJK/Hangul as one token
per rune. Budgets are therefore conservative for non-Latin content but may
still differ from the provider's tokenizer; set budgets below the model's
hard limit.
