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
  `fact.quota` makes the extractor's batch cap a decision rather than a cut:
  the prompt names the cap and every fact carries the extractor's own
  priority, so a batch over the cap keeps the claims that stand alone; see
  *Extraction density* below.
- Documents: canonical revisions are scanned through their scope-wide outbox
  cursor, chunked by the deterministic knowledge line, published as a
  document hierarchy, and reconciled into the projection lanes.
- Summaries: derived facts compact into immutable L0-L3 summary records with
  an active manifest served as its own retrieval lane. A pass extends the
  published tree with the facts its commit published instead of recompacting
  the conversation's window, and falls back to the complete window when a
  commit changes a fact the tree already summarises; see *Summary compaction
  (per commit)* below.
- Resident profile block: one bounded block per conversation, holding the host's
  explicit lines and the lines a maintenance pass distilled from the facts, and
  served on every context request as its own source class. It is off by default;
  see *Resident profile block* below.
- Trigger: the write paths append a **work record** to a scope-level journal
  when a turn, a document revision or a published fact lands, and the
  background runner drains it -- deriving the conversations the records name
  within a poll instead of within the sweep's period. The journal is a queue,
  not a log of authority: a record that does not land costs latency, and the
  sweep stays the fallback that derives everything the journal never saw; see
  *Derivation trigger (host-invoked runner)* below.
- Diagnostics: `Assembly.Diagnostics` reports per-scope derivation cursors,
  pending work, and cumulative worker counters, together with the state of the
  scope's projection lanes -- the producer version each lane reads and writes,
  whether a rebuild would change what it answers with, and the names its family
  holds state under -- the state of its maintenance overlay, and the trigger's
  cursors, claim and counters.
  `Assembly.LaneStatus` and `Assembly.OverlayStatus` are those two reports for
  one scope; context calls feed `memory.context.requests` /
  `memory.context.items` OpenTelemetry counters when the host initializes
  telemetry.
- Repair: `Assembly.Repair(ctx, RepairRequest{Scope, Lanes})` re-indexes named
  projection lanes from canonical state and reports what each one published.
  No deriver runs, so converging a lexical lane costs no model call; see *Lane
  repair (host-invoked)* below.
- Projection storage reclamation: a lane publishes immutable bases and
  segments and keeps one manifest as the only pointer, so what a compaction or
  a rebuild replaced is collected -- on the publication itself, keeping one
  generation of grace, and by the maintenance pass down to the active manifest;
  see *Projection storage reclamation* below.
- Retrieval: the context path fuses the recent lane with the configured
  projection lanes (BM25, entity, and vector when an embed model is
  configured), hydrates candidates, and packs them within the request
  budget.
- Maintenance: `Assembly.Maintain` decays and soft-merges superseded facts on
  the read path under an overlay stamped with the pass that wrote it -- one
  bucket per conversation, so a context call reads the conversations its
  candidates name rather than the scope;
  the decay clock reads from a fact's last recall as well as its event time,
  because the read path records a receipt for every long-term item a context
  result actually returns. `Assembly.RetireGenerations` drops the derived
  generations a retention policy no longer keeps -- their facts, lane entries,
  summary bookmarks, and the derivation progress that would otherwise resume
  in the middle of them. Both are host-invoked through the same assembly SPI;
  the module ships no runner.
- Forgetting: `Assembly.SoftForget(ctx, scope, addresses, reason)` records the
  named items -- a fact, a summary record, a document record -- in a per-scope
  ledger (`forget/v1/...`), and the read path stops serving them: the context
  request drops them in every lane, and a parent chain ends where one of them
  sits. `Assembly.RestoreForgotten` deletes the records and the items are
  servable again; `Assembly.Forgotten` reads the ledger, and `Diagnostics`
  reports how much it holds. Hiding is unreachability, not erasure: nothing is
  removed, and a record that quotes a forgotten item is a different address
  that stays servable until the host forgets it too -- see *Soft forgetting*
  below.

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
  cache:
    max_bytes: 2147483648
  compaction: {max_segments: 64, min_delta_bytes: 4194304, ratio: 2}
retrieval:
  rerank: false
observed_at_anchor: true
interval: 1m
derive:
  trigger: {interval: 1s, lease_ttl: 30s, batch: 256}
```

`observed_at_anchor` is on by default. With it, the derivation source renders
each message's `observed_at` anchor -- the time the message was observed, which
the host stamps on a live turn and an importer takes from the source's own
session -- as a text prefix (`[2026-09-18T12:30:00Z] Alice: ...`), so a relative
time reference resolves against when the message was observed instead of being
reconstructed from the conversation alone. The same prefix labels the raw
message evidence an answer model reads. A message without the anchor keeps its
untimestamped text: borrowing the commit time would date a bulk import to import
day. `observed_at_anchor: false` restores the untimestamped reading; the switch
is part of the derivation policy digest, so changing it re-derives the scope.

`generate` and `embed` are optional: without them the assembly serves the
recent and lexical/entity lanes. `interval: "0"` disables the background
runner while `RunOnce` stays available. Decay, consolidation, and forgetting
are host-owned maintenance jobs: the module keeps the canonical log and
derived views correct, and hosts schedule whatever hygiene they need.

`generate.temperature` pins the sampling of every generate call the module
makes -- fact extraction, summaries, and reranking. Absent, the provider's
default applies, which is what a product host usually wants; a measurement host
pins it, because an unpinned derivation is a draw. On this project's own corpus
two fresh derivations of one revision wrote 3424 and 3090 facts, and the fact
sets differed enough to move evidence recall by 1.4pp -- more than the effect a
read-path arm is asked to show. The pin changes what the extractor writes, so it
is part of the derivation policy digest and switching it re-derives the scope.
`embed` takes no temperature: an embedding is not sampled.

`derive.trigger` tunes the fast path beside the sweep, and the same
`interval: "0"` that turns the runner off turns the trigger off with it:
`derive.trigger.interval` alone stops the polls and leaves the sweep as the
only runner. `lease_ttl` has to outlast one conversation's pass and is the
claim every runner over the workspace shares, so runners that poll the same
workspace have to agree on it; `batch` bounds how many records one poll reads
per source.

`fact.quota` and `fact.max_facts` bound what one commit may add to a
conversation, and `fact.max_facts` is the lever that makes the quota bite
(`quota: true` with the default cap of 64 changes the prompt and nothing else,
because no measured batch reaches it):

```yaml
fact:
  max_facts: 16
  quota: true
```

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
- `views/fact/v2/...` — derived facts per generation, each conversation's
  published-generation pointer, and the canonical-identity area of every
  generation (`generations/<generation>/hashes/<canonical hash>/<fact id>`):
  which fact of that generation holds a claim.
- `views/summary/v1/...` — immutable summary records, the active record
  catalog, and the manifest bookmark of every generation that published one.
- `maintain/v2/overlay/...` — one control record per scope: the stamp of the
  pass that judged it, how many entries its buckets hold, and when it last
  judged the whole scope.
- `maintain/v2/buckets/...` — one document per conversation: the read-path
  decay factors and supersede marks the last maintenance pass wrote — each
  supersede mark with the relation it read and the window the fact was current
  over — stamped with the pass that judged them, so a context call reads the
  buckets the candidates' conversations own. A document in the pre-shard layout
  (`maintain/v1/overlay/...`) is never read: the status reports it as ignored
  and the next pass replaces it.
- `recall/v1/...` — the read path's receipts: one record per recalled item
  (count and last recall), read back by the maintenance pass.
- `forget/v1/...` — the soft-forget ledger: one record per forgotten item, on
  the identity the read path looks up, with the reason and the time the host
  gave. The read path scans its keys -- the identities -- once per request.
- `worker/v1/triggers/<partition>/journal` — the scope-level work journal: one
  append-only record per derivation a write path owes, keyed by the durable
  item it names (a commit, a document version) so a retry lands one record.
  It is a queue: nothing verifies its completeness, and the sweep is what
  derives whatever it never saw.
- `worker/v1/triggers/<partition>/cursors/<source>` — one consumer position
  per source, so a runner that restarts resumes at the records it had not
  derived instead of at the beginning of the journal.
- `worker/v1/leases/<partition>` — the scope's claim: the runner holding it,
  when it took it and when it expires. It is written back as released rather
  than deleted, so a host can still read who last held the scope.
- `projections/<family>/v3/partitions/<runtime>/<user>/<agent>/indexes/<name>/shards/<shard>/...`
  — rebuildable BM25 / entity / vector lane state, one addressed unit per
  conversation (`c:<conversation-id>`) or dataset (`d:<dataset-id>`): one
  `active.json` manifest per shard, the immutable bases and segments it names,
  and nothing a manifest stopped naming (see *Projection storage reclamation*).
  A shard is what a delta writes and a selector reads; a scope-wide rebuild
  publishes the shards its artifact set covers and drops the rest. The v2
  whole-scope layout is not read (memory has not shipped, so this is a break,
  not a migration). A vector entry's payload is base64 of the
  components' little-endian float32 bytes, not a JSON array of numbers: the
  stored bytes are what a lane's digests are computed over, and a decimal
  rendering nobody owns made them depend on which toolchain compiled the
  reader (#654). It costs 5.33 bytes per dimension instead of 14.1.
- `worker/v1/watermarks/...` — policy-scoped derivation cursors, retired with
  the generation they belong to.

## Query selectors

A query names what it wants to read with two selectors, and each one decides
which kinds of state it can reach: a named selector narrows its own family, and
the other family does not pass at all.

| Request | Reads |
| --- | --- |
| `conversation_id` only | that conversation's messages and facts |
| `dataset_ids` only | those datasets' documents |
| both | both of the above |
| neither | every shard in the scope (global recall) |

The shard plan is the same rule seen from storage: a conversation request reads
`c:<id>` (plus `d:<id>` for each dataset it names), and a request with no
selector reads all of them. The two axes used to be independent -- a request
that named only a conversation also read every document in the scope, because
the dataset axis was the absent one and nothing about documents was narrowed.
A caller that chats and queries a knowledge base therefore names `dataset_ids`
on the request; seeing the library by accident is no longer a read a request
gets. The semantics are stamped into the retrieval summary as
`selector_semantics: strict-v1`, so two runs are comparable only when their
selectors meant the same thing.

Summaries and the resident profile block are not projection state: a summary
request narrows by its own per-conversation manifest and the profile block is
served whole, so neither takes part in the plan.

A global recall is exact rather than approximate: every shard in the scope is
read, the merge is over the same scoring set a single table would have given
it, and nothing is truncated to bound the fan-out. It is the expensive path by
construction -- it reads what the scope holds -- and BM25 pays it in two passes
(set statistics first, then per-shard scoring) so the memory it needs is a term
table and a top-k heap rather than the whole corpus. A plan of one shard is
read once and scored from that reading, so a conversation query is atomic the
way a single-table read was; only a plan of several shards settles the set
statistics first and re-reads for scoring, where a write that lands between the
two passes can weigh a record against statistics that did not count it.

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
- A fact's canonical identity is indexed beside it, inside the generation
  (`hashes/<canonical hash>/<fact id>`), because it is the question every add
  asks: is this text already a fact here. A claim derived twice merges into the
  fact holding it instead of writing a second one. The area answers the
  question by name, so an add no longer lists the generation and reads back
  every fact it listed -- on the workspace substrate an add into a 1600-fact
  generation took 676 ms before the area and 6.3 ms after
  (`BenchmarkAddDistinctFact`), and a 1600-fact derivation pass holds ~155
  facts/s instead of falling with the corpus (`BenchmarkDeriveFacts`).
  - The entry is written before the fact it names: an entry without its fact is
    dropped by the next lookup that reads it, while a fact without its entry
    would be invisible once the area reports itself complete.
  - The area carries a marker key that says a build maintaining it populated
    it. A generation written before the area existed has none, so its first add
    scans once, records every identity it finds, and only then marks the area;
    every add after that trusts an absent identity instead of rescanning.
  - Retirement drops the area with the generation, so an identity cannot
    outlive the fact that holds it, and a replacement generation cannot inherit
    one.
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

The retrieval lanes tokenize with the shared kernel (`core/utils/bm25`), the
same splitter the tool catalog searches with: ASCII splits into words, CJK into
single characters plus adjacent bigrams, so a Chinese query matches a document
that mentions it without a segmentation dictionary. (The catalog additionally
ignores single-rune ASCII terms -- see `docs/guides/tool.md`.) Tokenization is
not part of the lane identity, and the policy digest does not carry it either,
so nothing re-derives a workspace because a splitter changed. Entries an earlier
splitter wrote stay where they are, and what it leaves behind depends on the
lane. The BM25 lane persists its terms, so a CJK document it indexed as one long
word is out of reach of the characters and bigrams a query produces now;
rebuilding the lane, or re-deriving the generation, is what restores it, and
`Assembly.Repair` is the host call that does it (`FullRebuild` is the
lane-side entry point it drives). The entity lane re-tokenizes its stored keys
on every search, so a stored key keeps matching as long as its token stream is a
contiguous run of the query's; a key several tokens long (whitespace or
punctuation inside it) still fails to match, as it always did. Such a key also
stays a vocabulary entry of its own and splits the IDF weight of the same entity
written both ways. ASCII is unaffected in both lanes: the two splitters agree
token for token.

One consequence of the splitter is worth knowing before turning it on for an
existing workspace: a CJK character contributes up to three terms (itself and
two bigrams), so the same character count reads as a longer document to the
length normalisation, and the fusion lane's query-length bucket is chosen by
token count -- a CJK query can land in a different bucket than it did under the
previous tokenizer and cross a deployment's configured `min_score` in either
direction. The BM25 lane is unaffected by that part: it scores with prefix
matching off and matches persisted terms exactly.

The algorithm versions of the lines this changed name the splitter they were
built with (`okapi-bm25-v2`, `deterministic-entity-v3`, `maintain-v4`,
`bm25-query-sigmoid-v2`). The last rides on every candidate's score term as its
calibration version (`fusion.BM25CalibrationVersion`), so a consumer can see
which calibration produced a score. The other three are read where the state
they wrote is read: the bm25 and entity versions are part of the producer
version their lane's manifest carries (`okapi-bm25-v2+k1=1.2,b=0.75` -- the
scoring parameters a persisted build carries are part of it too), the
maintenance version is part of the stamp on the scope's control record, and both
`verify.Plan` and `Assembly.Diagnostics` report a lane whose stored stamp the
running lane does not write, because a rebuild would change what it answers
with.

Nothing auto-rebuilds and nothing re-derives on that report: the lane keeps
serving what it holds, the read path ignores an overlay another pass wrote, and
carrying a workspace across the change is an operator's call -- which
`Assembly.Repair` is now the supported way to make.

One lane does not report its staleness that way, and it is the one whose
storage name is a function of its producer identity: the vector lane writes to
`<projection>.vector-<digest>`, where the digest covers the algorithm version,
the payload form, and the embedding model (`generationProjection`). A version
bump moves the name rather than the stamp, so the lane reads a name nothing was
ever written under
-- it reports no stored state and no staleness, exactly like a scope that was
never indexed, while the generation the earlier producer filled sits beside it,
unread. `LaneDiagnostics.OtherGenerations` is what says so: the other names the
lane's family holds state under for the scope, which for a deployment that
changed its projection setting or embedding model is the name it left behind.
`Lanes: ["vector"]` in a `RepairRequest` is what fills the name the lane reads;
the earlier generation stays where it is, unread and not reclaimed.

## Derivation trigger (host-invoked runner)

The sweep is the derivation runner's floor: it walks every scope's every
conversation on `interval`, which is what makes a commit retrievable within a
minute and an import retrievable eventually. The trigger is the fast path
beside it. A write path appends a **work record** to the scope's journal when
the durable item it describes lands -- `CommitTurn` names the conversation it
committed to, `PutDocument` names the document version, and the fact view names
the conversation whose generation gained a fact -- and `Assembly.RunTriggerOnce`
drains the journal: it runs the document pass for a scope whose document stream
advanced and one derivation pass per conversation a record names. With the
default `derive.trigger.interval: 1s` a committed turn is retrievable within
about a poll; the passes are the same `ProcessConversation` and document passes
the sweep runs, so a conversation derived by either is one the other has
nothing left to do for, and the watermarks and the published generation say so.
The drain's report names the conversations it derived (`ConversationIDs`),
which is the change signal a host needs to maintain what moved -- handing them
to `maintain.Service.RunConversations` refreshes the overlay of exactly those
conversations instead of re-judging the scope.

The record is a trigger, not an instruction: it carries no payload, the work it
asks for is idempotent, and a record read twice asks for work that is already
done. Recording is best effort -- a record that does not land costs latency,
never correctness, and the assembly reports the failure through diagnostics
rather than failing the write that owed it. The sweep stays what it was: the
fallback that derives everything the journal never saw, which is an importer
writing to the message store directly, a record that failed to land, or a
workspace an older version of this module wrote.

Three kinds of record exist. A **turn** names a conversation whose messages
were committed. A **document** names the scope whose document stream advanced:
documents are scope-scoped, so one pass clears every document record of a
drain. A **publication** names a conversation the fact view published a fact
for -- an import, a backfill, a host writing facts directly -- which is how
such a fact becomes indexed rather than waiting for the sweep. The view
publishes every fact it stores, the ones a derivation pass just wrote included,
so a pass leaves one more pass behind it: that pass derives nothing (the
commits are behind the watermark, the generation is already published) and
publishes nothing, so the polls settle instead of feeding themselves.

### The claim and the cursors

Runners are shared. `worker.LeaseStore` hands one scope to one holder at a
time, so two runners over one workspace do not derive the same conversation at
once: a drain takes the scope's claim, renews it as it derives, and stops when
another holder has taken it -- keeping the passes it ran, and leaving the rest
of the batch to the new holder. A claim that is not renewed expires
(`derive.trigger.lease_ttl`), so a crashed runner does not hold a scope
forever. Runners have to agree on the clock as well as on the ttl, because a
claim is only expired relative to each runner's own notion of now.

Each source has one durable cursor per scope, and a poll advances it over the
longest prefix of its batch whose work is done: a conversation another pass is
already deriving, or one whose pass failed, is left where it is for the next
poll -- and so is every record behind it, since one cursor cannot skip a
record. A pass that fails does not stop the drain: the conversations after it
are derived too. `Assembly.Diagnostics` reports each source's cursor and
whether it has pending work, the claim stored for the scope, and the trigger's
cumulative counters.

## Summary compaction (per commit)

Every published fact is a leaf of an immutable L0-L3 tree, and the
conversation's active manifest names that tree. A pass used to compact the
conversation's complete fact window after every commit, which made the branch
cost the conversation instead of the commit. A pass now **extends** the
published tree: it appends the facts its commit published to the leaves and
re-derives only the groups those leaves fill, keeping every group below the
change by identity -- records are content-addressed, so naming a group again is
the whole of the work. What an extension reads is the manifest, the tail group
of every level and the children those groups share with the rebuilt region.

Two things send a pass back to the complete window, and `SummaryRebuilds`
counts them. A commit that changed a fact the tree already summarises -- the
fact view merges identical content into the fact that holds it, so the fact's
record is no longer the one the tree names -- has to rebuild the groups that
fact sits in; and a tree the extension cannot place the commit's facts in
(another generation's, one published before the tree was recorded, a window the
facts do not continue) is compacted afresh, which is always correct. Everything
else counts as a `SummaryExtension`. A pass that derives what is already there
-- `derive: fresh` re-deriving a conversation -- extends nothing and rewrites
nothing: the manifest it leaves is the manifest it found, publication time
included.

`FactStore.List` pages the same way: a page is chosen from the identities and
creation times the scan already carries, and reads only the facts it returns,
so paging through a conversation costs each fact once instead of costing the
whole conversation on every page. The scan itself still walks the area it
lists -- the storage contract has no bounded range read -- so a page bounds the
work behind it, not the bytes in front of it.

## Lane repair (host-invoked)

`Assembly.Repair(ctx, RepairRequest{Scope, Lanes})` re-indexes the named lanes
of one scope from canonical state: the messages of every conversation, the facts
of the active generation, and the chunks of the active document builds, through
the artifact shapes the derivation worker writes. No deriver runs, so converging
a lexical lane costs no model call. An empty `Lanes` list rebuilds every lane
whose rebuild needs no inference -- the vector lane is rebuilt only when it is
named, because re-indexing it embeds every artifact again -- and a name that is
not configured is a validation error rather than a silent no-op. The result
reports, per lane, how many artifacts were indexed, whether the state the lane
served was written by another producer (`LaneRepair.StaleBefore`), and the
digests of the state that replaced it.

A repair holds the scope against derivation for its whole run, and derivation
refuses while it runs: a pass that indexed beside the rebuild would leave entries
in a lane the rebuild is about to replace. Both refusals are retryable and mean
nothing was rebuilt; a repair that fails halfway reports the lanes it finished,
and running it again is safe.

It is a re-index, not a re-derive: the derivation watermarks, the published
generation, the policy digest, and the overlay stay where they are. The cost is
one full re-index of the scope in memory, because a rebuild replaces a lane's
state in one publication. The message log is read in pages of the worker's page
size, the facts of the active generation and the chunks of the active builds are
read whole (that is the shape their readers offer), and the artifact set is
handed to every lane as it is rather than cloned per lane -- `ProjectionRequest`
documents `Artifacts` as read-only for that reason.

## Projection residency

One store keeps each lane's decoded state, and the lanes hold none of their own:
a query asks the store for the shard tables it needs, and the store answers from
the bases it has decoded or decodes the active state and keeps the result.
Because a conversation's table is not another's, the bound is in bytes rather
than in tables: `lanes.cache.max_bytes` (2 GiB by default) is what one lane's
store may keep decoded, weighed by the lane's own reading of a table -- the
vector payload for the embedding lane, the postings for BM25, the normalized
keys for the entity lane. The base read least recently makes room, and one base
larger than the whole budget is read, scanned, and dropped rather than held.
A budget never changes what a query sees: it decides what the lane keeps after
reading it, and what a bound costs is a decode of the base it evicted. The
readout is `vector.Stats.Resident` -- tables, bytes, hits, misses, evictions,
and uncached reads.

## Projection compaction

A delta appends a segment; a compaction folds the base and every segment into
a new base. When that happens is `lanes.compaction`: a shard compacts once its
segment count exceeds `max_segments`, or once the bytes appended since its base
reach the larger of `min_delta_bytes` and `base_bytes / ratio`. The byte
trigger is a share of the base rather than a fixed size because a fixed one is
wrong at both ends -- it rewrites a 10 MB conversation shard on a fraction of a
percent of its size, and it lets a large shard accumulate a rewrite-sized
segment set. At the defaults (64 segments, 4 MiB, base/2), a conversation
compacts after roughly its own size in deltas, which keeps the write
amplification geometric in what it stores instead of linear in how often it
writes. The policy changes how often a lane writes a base, never what a query
reads, and it is part of the retrieval summary so two deployments that compact
differently do not share a retrieval fingerprint.

## Projection storage reclamation

A lane writes immutable **bases** and **segments** and keeps one `active.json`
manifest as the sole pointer at the state it serves. A delta appends a segment
and names it; a compaction folds the base and every segment into a new base and
moves the manifest; a rebuild does the same from canonical state. What the old
manifest pointed at stops being reachable at that instant -- nothing reads it
again, because nothing but the manifest can find it -- and until this existed
nothing removed it either, so a lane's storage grew with the product of how
often it compacted and how large it was: one measured workspace held 9.4 GB in
its three lanes, of which 9.0 GB was state no manifest named -- the vector lane
alone held 8.8 GB against the 284 MB it served, and a second workspace repeated
it at 6.9 GB against 252 MB.

Three sweeps collect it, all of them deleting unreachability rather than
authority -- a rebuild writes again what a sweep removed:

- **On publication.** A base publication sweeps the lane's two object areas and
  keeps what the new manifest names, plus the generation it just replaced: a
  reader that resolved the previous manifest is allowed to finish, and a lane
  holds two generations at most. The grace generation costs no disk the
  compaction did not already hold, since it is the state that compaction read.
- **In the maintenance pass.** `Assembly.Maintain` reclaims each lane down to
  the active manifest — no grace generation — beside the superseded document
  builds it already reclaimed, and reports the count as
  `Plan.ReclaimedProjectionObjects`. It is what cleans a workspace an earlier
  build filled, and what an eval run's `-maintain` pass reports on its own log.
- **Per scope, without the pass.** `Assembly.ReclaimProjections` runs the same
  sweep over every registered lane of one scope and reports a
  `ReclaimResult` (lanes swept, bases, segments, legacy keys). It is the pass's
  sweep with
  nothing of the pass: no facts are listed, no overlay or profile block is
  written, so a host reclaiming a workspace an earlier build filled does not
  pay for judgments it is not making — and a workspace a report was measured on
  keeps the overlay that report read. `memory-eval -reclaim` is the command
  form, and it is what sweeps a store no maintenance pass is due on.
- **Per lane, when a host has to.** `component.Reclaimer` is the lane-side
  entry point (`ReclaimSuperseded`), implemented by the bm25, entity, and vector
  lanes; it delegates to the projection store's `Reclaim`.
- **The layouts this build does not read.** Each lane sweeps a second time, for
  a tree no sweep above can reach: the one an earlier projection layout left
  under the same scope (`component.LegacyLayoutReclaimer`). A layout is a
  segment of every key — `projections/<family>/<version>/partitions/...` — so a
  build that bumps it writes a tree beside the old one and reads only its own:
  the lanes name keys inside the version they write, and `OtherStoredNames`
  lists names inside it too, so the tree under the replaced version is storage
  no caller can name. The sweep deletes the scope's own partition under each
  listed version and reports it as `ReclaimResult.LegacyKeys` /
  `.LegacyLayouts` (`memory-eval -reclaim` prints both). A version that is not
  listed is left alone, which is the guarantee a rollback needs: the list names
  the versions this build has decided no build in it can read, and a rollback
  to one of those rebuilds from canonical state like any other layout change.

The manifest sweep is scoped to the name the lane reads, and to keys under
`bases/` and `segments/`: it deletes what it can compare against a manifest,
which is why the layout sweep is a separate half rather than a wider prefix.
Another name of the same family is another projection — the vector
lane names its storage after the embedding model it was built with — and a
deployment still serving that generation, or a rollback to it, reads what a
sweep would delete; `LaneDiagnostics.OtherGenerations` reports it instead of
removing it. A manifest the sweep cannot decode is refused rather than read as
empty: the sweep's question is what the manifest names, and a corrupt one has
no answer. Like every other publication to a lane, a sweep expects the lane's
single writer — the state a concurrent writer is publishing is not in the
manifest the sweep read.

`ReclaimResult` counts objects rather than bytes: a sweep deletes by key, and
measuring what it removes would read the storage it exists to give up. A host
that wants the size stats the workspace it owns.

The summary view's own catalog (`views/summary/v1/...`) publishes the same way
— immutable base, delta segments, one head — but is not swept here, and a
workspace pays for it in kilobytes rather than gigabytes: its head is
bookmarked per generation, so every generation a retention policy keeps names a
chain that stays readable, and a sweep could only collect the objects a
compaction inside one generation replaced. A measured workspace held 33 of them
(80 KB) against 9.0 GB of lane snapshots.

## Integrity and evaluation

- `Assembly.Verify(ctx, scope, conversationID)` runs the repair evidence
  checks read-only: dangling fact links, source digest drift, summary
  digests, and projection digest drift. It also reports a lane whose stored
  producer version is not the one the running lane writes, as a `rebuild`
  action. What it cannot see is state under a name the lane no longer reads: a
  forked generation has no manifest to audit, which is why that case is
  reported by `LaneDiagnostics.OtherGenerations` instead.

Lane manifests carry the producer version of the state they point at, and that
stamp is deliberately kept out of the identity a manifest is checked against: a
build that predates the field recomputes the same identity, so a rollback (or a
mixed-version deployment over one workspace) keeps reading the manifests a newer
build wrote instead of reporting every one of them corrupt. The stamp is
compared on its own, by `verify` and by `Assembly.Diagnostics`.

## Extraction density

One chat commit derives 12.59 facts on average over the benchmark workspace this
project measures against (3424 active facts over 272 commits; 99th percentile 27
facts in one commit, maximum 38), while a retrieval pack holds 30 items in that
measurement and 20 items by default. Most derived facts are therefore never
retrieved, and a turn that produces a dozen claims competes for the same slots as
every other turn.

`fact.quota` turns the batch cap into a decision instead of a cut: the extractor
prompt names the cap (`fact.max_facts`) and asks every fact for its own priority
("high" for a claim a question can be answered from on its own, "low" for one
that only carries its meaning beside another fact of the turn), and a batch over
the cap is truncated by that priority instead of by position. It changes what the
extractor writes, so it is part of the derivation policy digest and switching it
on re-derives the scope. It only bites where the cap is set deliberately: at the
default 64 no measured batch comes close, so the quota's effect there is the
prompt's own instruction to spend the turn's slots on claims worth retrieving.

The cheaper-looking lever -- folding the near-duplicate facts of one commit
together -- is deliberately not in the module, because the measurement says there
is nothing to fold. `eval/cmd/memory-fact-density` reads a stored workspace and
reports what a similarity merge and a length floor would actually remove:

```
$ cd eval && go run ./cmd/memory-fact-density -workspace ./workspace -runtime memories
stored facts 3424 over 272 commits (12.59 facts/commit)
facts per commit: p50=12 p90=19 p99=27 max=38
same-commit pairs 23161 (canonical duplicates excluded)
jaccard histogram (0.0-0.1 ... 0.9-1.0): [5451 10577 6029 930 142 24 6 2 0 0]
containment histogram (0.0-0.1 ... 0.9-1.0): [1090 3059 5363 6363 4272 2237 596 141 35 5]
projection (floor + merge, greedy, longer text survives):
  jaccard     cutoff 0.50  survivors  3392 (kept 99.1%)  12.47 facts/commit
  containment cutoff 0.85  survivors  3411 (kept 99.6%)  12.54 facts/commit
```

Two thirds of the same-commit pairs score below 0.3 by token Jaccard, and the most
similar pairs that do exist are not duplicates: they are the two sides of one
event ("Calvin asked Dave to take pictures of his trip" / "Dave agreed to take
pictures of his trip"), which downstream questions ask about separately. Pushing
the threshold down to where it folds anything at all starts folding distinct
claims. The density is made of distinct claims, so the lever that reduces it is
asking the model for fewer, better ones -- and that is a recall trade to measure,
not a free win.

## Maintenance (host-invoked)

`Assembly.Maintain(ctx, scope)` runs one soft-merge and decay pass: it detects
superseded facts (same entity plus similar text, or the same single-valued
relation about the same subject; the newer fact wins) and aged
facts (exponential decay by event time or last recall, whichever is later),
writes a per-scope read-path overlay, reclaims the derived document builds the
scope no longer serves, and returns the plan. Canonical fact text is never
edited — the overlay only multiplies retrieval scores, and the strongest of the
supersede and decay factors wins. The module ships no runner: hosts schedule
the call however they want (cron, queue, or an admin endpoint). Tuning lives in
`maintain.Config` (similarity threshold, supersede factor, decay half-life,
decay floor, recall saturation, relation vocabulary).

The derivation trigger is not a maintenance runner: it derives what a write
path owed. A host that wants the overlay refreshed for what changed reads the
conversations a drain derived and hands them to
`maintain.Service.RunConversations`, which rewrites those conversations'
buckets and leaves the rest of the overlay as the last whole pass measured it;
see *Derivation trigger (host-invoked runner)* above.

The overlay is per conversation: a scope's control record names the pass that
wrote it and how much it holds, and each conversation's factors live in their
own bucket. A context call therefore reads the buckets the conversations of its
candidates own rather than the scope's whole overlay, and a pass that judges a
subset of the conversations replaces exactly that subset
(`maintain.Store.SaveConversations`; `Assembly.Maintain` stays the whole-scope
pass). Supersede judgments never cross conversations, so re-judging the
conversations that changed judges what a whole pass would judge them to be;
decay is always recomputed from the pass's own clock, so a conversation a
partial pass leaves behind keeps the factors the last whole pass measured.

### Supersede: two rules, one window

A fact is superseded when a newer fact of its conversation replaces it, and the
pass decides that two ways. The similarity rule reads the texts: facts sharing
an entity whose token overlap reaches the threshold are restatements of one
claim, and the older one is the earlier restatement. The relation rule reads the
predicate the extractor wrote: two facts stating the same single-valued relation
about the same subject cannot both be current, so the older one is replaced by
the newer even when the two texts share almost no tokens — the "I moved" update
that similarity cannot see. Each finding names the rule that decided it
(`Plan.Supersedes[].Reason`) and, for a relation, the relation it read
(`.Relation`), and carries the token similarity it measured either way: a
relation finding with a low similarity is this rule's whole evidence.

The relation rule is deliberately narrow. A relation joins the vocabulary only
when one subject can be in it one way at a time, and `maintain.DefaultRelations`
holds the profile relations that qualify — where someone lives, who employs
them, what they work as, who they are married to, and where and when they were
born. Preferences, events, plans and family-of relations all stay current
together however a model phrases them — two facts about a pottery class and a
choir rehearsal are two classes, not an update — so they are not in it, and a
phrase the vocabulary does not recognize leaves the pair to the similarity rule:
a missed update, never a wrong one. A predicate is matched by its folded form
(case, `_`/`-` separators and stray punctuation are the model's spelling, not a
different relation), the subject is read as the entity the fact's own text names
first, and the two facts have to state different values of the relation — which
the flat schema lets the pass decide only where they name different objects, so
"moved to Berlin" does not replace "lives in Berlin". A deployment whose facts
carry no predicates — the extractor writes them only under its `rich` strategy —
configures an empty vocabulary (`maintain.Config.Relations`), which turns the
rule off instead of digesting a vocabulary that can never match.

The vocabulary is part of what the pass is, so it is digested into the overlay's
stamp beside the tuning: the same facts judged under another vocabulary are
another pass's judgments, and this deployment serves only its own.

Both rules write one kind of overlay entry, and both carry the window the older
fact was current over. `ValidAt` is the superseded fact's own date — its event
time, or when the fact was created if the turn stated none — and `InvalidAt` is
the superseding fact's date, which is the same key the pass ordered the pair by,
so a window never runs backwards. `Plan.Supersedes[].ValidDetail` and
`.InvalidDetail` carry each side's time expression as the extractor stated it,
which is what a human checks the resolution against. The window is what answers
"what did we know at time T": a host reads it back with `maintain.Store.Load`
(or from the plan) and compares T against the interval. The read path itself
only multiplies the score, because a context request carries no time of its own.

Nothing here hides a fact or rewrites one, and that is deliberate: a superseded
fact keeps its row in the view, its text, its dates and its place in the read
path, served at the reduced factor (the strongest of its factors wins — see
below), so a question about the state before the update is still answered from
the fact that states it.

### Recall receipts

Every context request that carries a `RecallEventID` and returns long-term or
summary items leaves a receipt: a per-item record of how often and how recently
the read path returned it (`recall.Store`). Only the retrieval provider writes
these, and only from a packed result, so an import — commit, derive, backfill —
produces none: importing history is not reading it, and a fact that was never
served decays like one that never was. The maintenance pass reads the receipts
back and both uses them and reports them: `Plan.Decays[].Recalls` /
`.LastRecall` name the receipt a compensated factor was measured with, and
`Plan.RecalledFacts` counts the scope's facts that had one, which stays zero
for a deployment whose receipts never arrive.

A recall compensates decay and never adds to it. The factor written to the
overlay stays in `[decay, 1]`: each receipt forgives
`count/(count+recall_saturation)` of the decay that remains, so a fact the
caller keeps being served is judged from its last recall and can climb back out
of the overlay entirely, but nothing outranks an un-decayed fact. The count is
a saturating heuristic, not a ledger: it is capped per item, a re-delivered
event is recognized by its id and not counted again, and two recalls of the
same item racing each other are allowed to lose one increment. Receipts are not
pruned: one small record per recalled item, re-read by each pass, and a record
whose item no longer exists is inert rather than removed.

The overlay is stamped with the pass that wrote it -- the maintenance algorithm
version together with that tuning (`maintain.Config.ProducerVersion`) -- and the
read path applies an overlay only when its stamp is the one this deployment's
tuning serves. An overlay written before the stamp existed, or under another
tuning, is ignored rather than applied to another policy's scores: canonical
facts are untouched either way, which is what makes a pass its own repair, since
the next one rewrites the scope's buckets under the running policy. A host that
wants to know before it schedules one reads `Assembly.OverlayStatus` (or
`Diagnostics`' per-scope overlay), and `Plan.IgnoredOverlay` says whether a pass
replaced judgments it did not apply. That status also reports `UpdatedAt` and
`Expired`: `UpdatedAt` is when the scope was last judged whole (a partial pass
keeps it), and an overlay the running pass wrote but has not refreshed within
one decay half-life carries factors measured against a clock at least that old
-- `Expired` is the host's signal to schedule the whole-scope pass.

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

## Soft forgetting (host-invoked)

`Assembly.SoftForget(ctx, scope, addresses, reason)` records items a host has
decided must not be served, and the read path stops serving them.
`Assembly.RestoreForgotten(ctx, scope, addresses)` deletes those records and the
items are servable again; `Assembly.Forgotten(ctx, scope, conversationID)` reads
the ledger (what, why, since when), and `Diagnostics` reports per scope how much
it holds. There is no setting: an empty ledger changes nothing, and one with a
record in it must not depend on a switch being set, so a deployment cannot serve
a forgotten item by configuration.

An address is the coordinates a context result carries -- `{kind, conversation,
item}` for a fact or a summary record, `{kind, dataset, document, item}` for a
document record -- and the ledger stores the identity the read path computes for
it (`corememory.ContextAddress.Identity`, qualified by the hard scope). The
ledger is therefore per address, and the address is what makes the read path's
behavior follow:

- Every lane the provider serves is covered by the same check: a forgotten fact,
  summary record, or document record is dropped wherever it was found, before
  packing and before a fact's source quotes are pulled in.
- A parent chain ends where a forgotten record sits. Hydration walks a
  document's hierarchy upward; walking past a forgotten section would serve the
  section's own text one level up, so the chain stops there. A record *below* a
  forgotten one is a separate item and is served on its own: forgetting a
  document's resource stops the walk when it reaches it, and leaves the chunks
  the query matched.
- The recent lane is never filtered: it serves the conversation's own last
  turns, and a forget covers what the views derived from them. The canonical
  transcript stays readable, which is a statement about this primitive, not a
  limitation of it (see below).
- A forget outlasts a re-derivation. The ledger is keyed by the identity, not by
  the generation that produced the item, and a derived id is content-derived
  rather than policy-derived (a fact from its text, a summary record from its
  inputs and transform, a document record from its provenance and content) -- so
  a policy change that re-derives a conversation derives the identities that are
  already forgotten, and the read path keeps dropping them. A forgotten item
  never comes back by being derived again; it comes back when its record is
  deleted.

**Unreachable is not unrecoverable.** A forget is a read-path overlay, and it
leaves everything where it is: the canonical messages and the Log entries they
live in, the derived facts and summary records in their views, the projection
lanes' entries, and every cache in front of them. `Assembly.Facts`,
`Assembly.Summaries`, `Assembly.Verify`, and `Assembly.Repair` still read all of
it -- an audit has to be able to see what is stored, and a repair must not be
blinded by an overlay -- so anyone with the workspace can read a forgotten item.
Erasing the stored copies is a different operation with a different cost, and
the module does not pretend the overlay is one: what a forget removes from an
answer is the item, and only while the host keeps the record.

What that means for records that quote a forgotten item: they are different
addresses and stay servable on their own. A summary that compressed a forgotten
fact, a document section holding a forgotten chunk, a higher-level summary over
the summary -- each is its own item, and forgetting the fact does not forget
them. A host that needs a quotation gone forgets the record that quotes it as
well; the ledger is what makes that auditable, since it names every address the
read path was told to drop, and `SoftForget` returns the records it wrote.

An overlay that cannot be read is not an empty one. A record this module cannot
decode, or a ledger read that fails, leaves the item *unserved* and reports the
failure as a request diagnostic; a request whose every candidate could not be
decided fails rather than answering like an ordinary memory with the overlay's
items gone quiet. That is the direction a forget has to fail in -- an entry
nobody can read was a statement that the item was meant to be unreachable --
and it is why the interface's `Visible` read has an error to return.

The cost on the read path follows the ledger, not the scope: a context request
reads the scope's ledger keys once (a key-only scan where the substrate offers
one, the records' values otherwise), and a substrate without a bulk read -- or
one whose bulk read failed -- falls back to one point lookup per candidate. The
ledger holds one small record per item a host decided to forget, so its bound is
itself, the same way maintenance's overlay is bounded by its own judgments. The
check itself runs on the hydrated item, which is the shape the provider's
overlay interface has: an item the ledger hides is hydrated and then dropped.

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

## Resident profile block

A conversation's **resident block** is a bounded set of lines about its
participants that every context request carries without retrieving them: what
someone loves, works as, has been doing, is working toward. Retrieval is
query-shaped and a profile is not -- a stable preference answers questions that
share almost none of its words -- so the block is the other half of that
contract: present whether or not the request resembles it.

```yaml
profile:
  enabled: true     # serve the block and let the pass fill it; off by default
  max_tokens: 512   # its bound in the module's cost model (ceiling 2048)
  max_entries: 32   # and in lines (ceiling 128)
```

It is off by default, and that default is a statement rather than an omission:
the block rides along with every request, so turning it on changes what every
answer reads. Its bound is what keeps it from becoming a second retrieval pool.
The bound holds whether or not the block is served -- every write is validated
against the caps before it lands, and a value above the module ceiling is
config-rejected rather than clamped.

Two writers share one block, and the split is the point:

- the **host's half** is what a host wrote through `Assembly.Profiles()`
  (`SaveExplicit` replaces it whole, and no pass rewrites it);
- the **pass's half** is what `Assembly.Maintain` judged from the
  conversation's facts: the lines whose text states a profile class
  (preference, identity, habit, goal) about one entity, newest first inside
  each class, restatements folded away, superseded facts left out -- the block
  states what holds now.

A line is a fact's own text, verbatim: the block selects and orders, it never
paraphrases. A distilled line names the facts it was judged from, and the pass's
half carries the policy that judged it (vocabulary, caps, dedupe threshold) as a
stamp: a stored half written under another policy is not served, so changing the
vocabulary hides stale judgments instead of serving them under a new name, the
way the maintenance overlay's stamp works.

Serving is an addition to a request, not a lane in it. The item carries its own
kind and source class (`profile`), is built after the lanes, the reranker and
the source quotes -- a query, a score floor or a dataset filter does not select
it -- and the packer admits it before its class reservations, so a retrieved
class filling its own share cannot spend the room the block was bounded to
occupy. A block the request budget cannot hold is not served at all rather than
cut: half a block is not the lines the host and the pass put there, and a line
dropped mid-block would read as a statement that changed. A host write that
would exceed the bound makes room by dropping the pass's lowest-ranked lines
(the next pass re-judges them) and is refused only when the host's lines alone
do not fit; a pass's half that does not fit is refused whole.

The block is a reading of the corpus, not a copy of it: **a hide reaches it**. A
distilled line whose provenance names a forgotten fact is dropped from the
served block -- restore the fact and the line is back, without a rewrite --
while the host's half is the host's own words and is not filtered. Nothing here
edits canonical state: the facts a line quotes keep their text, their dates and
their place in the read path.

The vocabulary is the module's measured default (`profile.DefaultClasses`): over
this project's own evaluated facts it matched 240 of 3828 (6.3%, 17-38 per
conversation), and what it matched reads as a profile: what each participant
loves, plays, works as, and is working toward. Events are absent on purpose:
"attended" describes a turn, not a person, and a class that admits them turns
the block back into a retrieval pool. A deployment whose corpus is not English
leaves the vocabulary empty or leaves the block off; the `memory.yaml` surface
carries no vocabulary of its own and `profile.Config` is the extension point.

The harness measures the block with two runs over one workspace: a deploy
document that enables it plus `memory-eval -prepare-all -maintain`, against the
same document without it. A run that enables the block without running a pass
says so on stderr, because a blockless read path must not be reported as the
block; `eval/README.md` documents the flag and what the fingerprint records.

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
