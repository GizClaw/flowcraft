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
  watermark, extracts facts (chat line) into the generation that policy owns,
  publishes that generation once the pass completes, and feeds every
  projection lane exactly once per commit.
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
- Maintenance: `Assembly.Maintain` decays and soft-merges superseded facts on
  the read path, and `Assembly.RetireGenerations` drops the derived generations
  a retention policy no longer keeps -- their facts, lane entries, summary
  bookmarks, and the derivation progress that would otherwise resume in the
  middle of them. Both are host-invoked through the same assembly SPI; the
  module ships no runner.

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
- `views/fact/v2/...` — derived facts per generation, plus each conversation's
  published-generation pointer.
- `views/summary/v1/...` — immutable summary records, the active record
  catalog, and the manifest bookmark of every generation that published one.
- `projections/...` — rebuildable BM25 / entity / vector lane snapshots.
- `worker/v1/watermarks/...` — policy-scoped derivation cursors, retired with
  the generation they belong to.

## Derivation generations

Everything derived from one conversation belongs to a **generation**, whose
identity is the policy digest: the derivation settings, the generate/embed
models, and the algorithm versions of every derivation line. The policy digest
already keys the derivation watermarks; it also names the fact generation, and
it labels the summary manifest, so one identity answers "which policy produced
this" for every derived view.

Facts are stored per generation, so changing a prompt or a model re-derives the
same canonical commits into a new generation instead of merging beside the one
it replaces:

- Derivation reads the generation it is building (link candidates, and the fact
  window summaries are compacted from); readers resolve the conversation's
  published generation and never name one.
- A pass publishes its generation when it finishes, so an interrupted pass
  leaves the previous generation visible instead of exposing a half-built one.
  A conversation with no published generation adopts the first generation
  written, so a fresh conversation stays readable while it is derived.
- Publishing a generation also converges the projection lanes with it, before
  the pointer moves: the facts the generation being replaced owns exclusively
  stop being projected, and the active window is re-projected when an earlier
  switch pruned it. A converge that fails leaves the previous generation
  visible and the next pass retries, so the lanes never describe a generation
  readers cannot resolve -- which is what makes re-derivation converge instead
  of leaving the two generations side by side in the lanes.
- Projected facts are addressed by conversation: a fact id is a content address
  shared by every conversation that derived the same text, and a lane is
  partitioned by scope, so the lane entry carries the conversation and
  `item_id` keeps the fact id the read side hydrates by.
- Re-deriving under the same policy converges: the visible facts, the stored
  generations, and the derived views do not grow.
- Rolling back is switching policy, not restoring a backup: building an
  assembly with the previous policy publishes the generation it already wrote.
  The summaries come back with it — the branch bookmarks the manifest each
  generation publishes, and the switch (which runs even in a pass that derives
  nothing, as a rollback does) serves that generation's own manifest again —
  rather than leaving reads with the summaries of the replaced generation or
  with none.
- Rolling back into a generation a **sweep** retired derives it again instead of
  resuming it: retirement drops the generation's derivation progress with the
  generation itself, so the pass starts at the head of the stream and publishes
  what it derives rather than the tail of what the sweep removed. It is the same
  switch over a generation that is no longer stored, and it is why the cursor is
  retired as part of retiring a generation rather than left behind as an
  afterthought.

Replaced generations stay stored, which is what makes a rollback possible, and
that makes retirement necessary. `Assembly.RetireGenerations(ctx, scope,
conversationID, keep...)` is the sweep: it drops every derived generation of one
conversation except the ones named, over the facts, the summary bookmarks, the
projection lanes, and the derivation progress of the generation itself, in one
call. One call because everything a generation is addressed by is the same
identity: dropping a generation's facts while its summary manifest stays
bookmarked leaves a rollback that serves summaries of facts that are gone, and
the derivation watermark is keyed by that identity too — a generation whose
facts are retired while its cursor stays at the end of the stream is one a later
rollback resumes in the middle, deriving the tail and publishing a generation
missing the rest. The sweep therefore keeps every generation a view is still
serving (so views left on different generations by a policy change are not
pruned while they disagree), it retires the derivation progress of the
generations it drops before anything else, then the lanes' entries for them, and
it retires the bookmarks before the facts (so an interrupted sweep leaves
unreachable facts, which the next sweep collects, rather than a bookmark to
facts that no longer exist).

The progress goes first because it is the only thing a sweep removes that is not
derived state: the cursor is where the next pass under that policy resumes, not
what any reader resolves, so dropping it first means an interruption leaves the
generation's facts without a cursor — which re-deriving under that policy
reproduces from the canonical commits, since their derived addresses are content
derived — rather than leaving the state the sweep exists to prevent, facts gone
under a cursor that still points past them. Retiring the progress of the
generation reads resolve is refused (`Processor.RetireProgress`): an ordinary
pass under a policy whose generation is visible would derive the stream a second
time, and a re-derivation is the model's, so the visible generation would grow
beside itself instead of converging. A sweep never names it — it retires what the
views do not serve.

The lanes are swept next because a lane entry is addressed by the id of the
fact it projects: once a generation's facts are gone those addresses can no
longer be enumerated — the converge walks the stored generations — so the lane
would keep offering a candidate the read path cannot hydrate, and nothing else
would collect it. The generation that reaches that state is one whose pass
failed after projecting: its facts are stored and its entries are in the lanes,
but it never published, so no switch reconciled them. Sweeping is a maintenance
action and expects derivation over the conversation to be quiesced, since the
generations to purge are listed before they are retired: a sweep run beside a
pass, or over the generation that pass has derived but not published, would
leave the pass publishing a generation whose facts and lane entries the sweep
removed, and derivation resumes after its own watermark, so the commits that
generation already covered are never derived again.

The sweep enforces that precondition instead of assuming it: it refuses while a
pass over the conversation is running, or while this worker has stored facts
under its own generation without publishing it, and returns
`worker.ErrDerivationUnsettled` (the same state is observable, before the fact,
as `Assembly.DerivationState`). Both are retryable, both mean the sweep retired
nothing, and both are answered by deriving again — `Assembly.RunOnce` is
synchronous — and sweeping after that pass. Commits no pass has scanned yet are
deliberately not part of it: a sweep only removes generations readers do not
resolve, so it is safe beside a lagging watermark, and the pass that catches up
converges the lanes with the generation it publishes.

The per-view sweeps stay available for tooling that retires one view alone:
`FactStore.ListGenerations` / `RetireGeneration` / `RetireGenerations` and the
same three on `SummaryStore`, whose `ListGenerations` reports the generations
that published a manifest. Retirement is unreachability, not erasure: the
merge-event streams live in the append-only Log, whose contract has no delete,
and a summary record is a content address shared by every generation that
compacted the same inputs, so a sweep removes the facts and bookmarks of the
retired generation and keeps the records the surviving generations read. They
leave the derivation progress where it is: `Processor.RetireProgress` is what
the sweep over one conversation retires the cursors with, and a caller that
retires a view on its own is the caller that has to retire the cursor of the
generation it removed.

A workspace written before generation scoping is not migrated: the fact view
moved from `views/fact/v1` to `views/fact/v2`, and a fact stored without a
generation cannot be assigned to a policy it never ran under. The stored
derivation watermarks are keyed by policy digest and survive the move, so a
workspace whose derivation settings did not change reports nothing to derive
while its `views/fact/v1` facts stay unread. Change a derivation setting, or
drop the watermarks under `worker/v1/watermarks/...` (which is what a sweep does
for the generations it retires), to derive the next generation. Its projection
entries stay where they are -- an entry derived before the lane identity changed
is addressed by the fact id alone -- and are replaced by re-deriving or by
rebuilding the lane.

## Integrity and evaluation

- `Assembly.Verify(ctx, scope, conversationID)` runs the repair evidence
  checks read-only: dangling fact links, source digest drift, summary
  digests, and projection digest drift.

## Maintenance (host-invoked)

`Assembly.Maintain(ctx, scope)` runs one soft-merge and decay pass: it detects
superseded facts (same entity plus similar text; the newer fact wins) and aged
facts (exponential decay by event time), writes a per-scope read-path
overlay, reclaims the derived document builds the scope no longer serves, and
returns the plan. Canonical fact text is never edited — the
overlay only multiplies retrieval scores, and the strongest of the supersede
and decay factors wins. The module ships no runner: hosts schedule the call
however they want (cron, queue, or an admin endpoint). Tuning lives in
`maintain.Config` (similarity threshold, supersede factor, decay half-life,
decay floor).

The reclaim is the document half of the sweep surface, and it is deliberately
not a generation sweep: see *Derived documents are not generation scoped*
below. `Plan.ReclaimedChunks` reports how many superseded chunk records one pass
removed, the overlay is saved before they are, and a retried pass finds less to
remove. A deployment without derived documents configures no document view on
the service and reclaims nothing.
- `eval.Run` drives a scenario (turns + questions) through the capability
  SPI and reports hit rate and latency. The harness is dataset-agnostic;
  LoCoMo/LongMemEval exports convert to scenarios host-side.
  `LoadLoCoMo` / `LoadLongMemEval` convert the upstream JSON (session
  ordering, role mapping, image annotations), `RunAll` executes a dataset,
  and `Baseline` records/compares hit rates for regression gates. The runnable
  CLI and the live-provider deploy wiring live in the sibling `eval` module
  (`backends/memory/eval/cmd/memory-eval`); this library module keeps no
  provider-driver dependency.

Retention is the other half of the same surface: `Assembly.RetireGenerations(ctx,
scope, conversationID, keep...)` drops the derived generations a policy no
longer keeps, across the views and the projection lanes that hold them and the
derivation progress that would resume in the middle of them, and reports what it
removed as `RetireResult`. It has no configuration — which generations to keep is
the caller's retention policy, named per conversation. See *Derivation
generations* above for what a sweep keeps, why the views are swept together, why
the progress and the lanes go first, and what makes it refuse a conversation
whose derivation has not settled.

## Derived documents are not generation scoped

Conversations are swept by generation; documents are not, and they do not need
to be. A document view is addressed by dataset and document
(`views/v1/documents/<partition>/<dataset>/<document>`), and a publish writes one
immutable **build** and moves an `active` pointer at it. A chunk id comes from
the document's provenance and text (`lines/knowledge`), not from the policy that
derived it, so deriving a document again under an earlier policy reproduces the
build it published then: there is nothing a rollback would need to find that
re-deriving does not reproduce, which is exactly why the view keeps no
generation and why `Assembly.RetireGenerations` — a per-conversation sweep —
never touches it.

What a document does leave behind is a superseded build: a new revision, a
re-derivation under changed chunking, or a publish that fails after writing its
chunks and before moving the pointer. `Get` and `List` resolve only the active
build, so nothing reads them, and the projection lanes are not a reason to keep
them either — the worker reconciles a lane's entries for a document when the
document changes. They are reclaimed at scope granularity, by the same
maintenance pass that soft-merges facts (`Assembly.Maintain`, reported as
`Plan.ReclaimedChunks`) or by `DocumentViewStore.RetireScopeBuilds` directly,
which keeps the active build of every document of the scope and removes the rest
— including the builds of a document whose pointer never landed, since none of
them has a reader. Reclaiming is unreachability, not erasure of authority: the
canonical revisions live in the document Log, and a build removed here is
written again by the next publish that needs it.

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
