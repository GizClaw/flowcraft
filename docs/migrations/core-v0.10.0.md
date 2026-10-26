---
layout: default
title: Migrating to core/v0.10.0
---
# Migrating to `core/v0.10.0`

`core/v0.10.0` is the follow-up to the v0.9.0 review. Most of it is about
what happens *after* something has gone wrong — a panicking tool, an aborted
reload, a credential store that lost an index entry — so a host that never
hits those paths sees no difference. The one structural break is on the graph
side: node types are deployment resources now, so every graph engine document
gains `node_type` deps and loses the collaborators it used to hand the engine.
Two document entries do change meaning, two Go API surfaces moved, and one
interface kept its shape while changing what it is keyed by.

## Graph node types are resources

`agent.Engine/graph` registers no node type of its own. `inference`, `tool` and
`script` — and every custom type — are `graph.NodeType` resources mounted on the
engine's `node_type` dep, and the collaborators they used to take from the
engine are now their own deps. A v0.9.0 document fails twice over: the engine's
settings reject `script_runtime_name` as an unknown field, and a graph naming
`inference` builds against a registry nothing filled —
`graph: node "chat": unregistered type "inference" (no node type is registered;
a graph node type is a graph.NodeType resource wired as a node_type dep)`.

Before:

```yaml
agents:
  assistant:
    engine:
      kind: agent.Engine
      impl: graph
      deps:
        inference: infer
        router: router
        tools: tools
        workspace: ws
        script_runtime: js
      settings:
        graph: {file: ./graphs/assistant.yaml}
```

After:

```yaml
resources:
  node_inference:
    kind: graph.NodeType
    impl: inference
    deps: {inference: infer, router: router, tools: tools}
  node_tool:
    kind: graph.NodeType
    impl: tool
    deps: {tools: tools}
  node_script:
    kind: graph.NodeType
    impl: script
    deps: {script_runtime.js: js, tools: tools, workspace: ws}

agents:
  assistant:
    engine:
      kind: agent.Engine
      impl: graph
      deps:
        node_type.inference: node_inference
        node_type.tool: node_tool
        node_type.script: node_script
      settings:
        graph: {file: ./graphs/assistant.json}
```

What moved:

- **Dep keys.** `node_type.<suffix>` (or plain `node_type`) mounts a type; the
  suffix is the name graphs reference, and the type's `settings.type` overrides
  it. Mount one type per capability set — a graph with no `script` node cannot
  run script once no script type is mounted, which is the point of the move.
- **Script runtimes.** `script_runtime_name` is gone from the engine settings.
  A script-running type mounts `script_runtime.<name>` deps, and a node's
  `config.runtime` selects one by that name, so one type serves a JS node and a
  Lua node of the same graph.
- **Script-backed types.** `graph.NodeType/script` is now `script_inline`: the
  source stays in the resource and `runtime` moved into its settings. The name
  `script` is the generic impl whose source comes from each node's config.
- **Where the checks live.** The engine never reads a node's config, so "an
  inference node without `model` needs a `router` dep" and "this node names a
  tool the catalog does not have" are reported by the mounted type at `Build`.
  Same failures, now naming the node, the type, and the dep to add; a graph
  naming a type nothing mounted reports which types exist.

`script_bindings` keeps its meaning: the engine-level provider still replaces the
surface for every script-running type, and without it each type assembles the
standard surface from its own deps.

In Go, `graph.NodeType[C]` gained an optional
`ValidateConfig func(nodeID string, raw json.RawMessage) error` — called at
`Build` with the config as written, `${board:*}` references unresolved — and
`graph.TypeNameProvider` is the optional interface behind the dep-key
diagnostics. A host or plugin that registers types through `graph.RegisterType`
needs no change; it just has to be mounted as a `node_type` dep now.

## Panic recovery is on by default

`tool.Assembly/middleware` now installs `Recover` unless the document turns it
off. A tool is foreign code — a plugin, an MCP server — and its panic is a bug
in that tool, not a reason to lose every session in the host:

```yaml
tools:
  kind: tool.Assembly
  impl: middleware
  settings:
    middlewares:
      recover: {enabled: false}   # opt out; on by default
```

The middlewares that stay opt-in are unchanged (`telemetry`, `timeout`,
`concurrency`, `result_limit`, `truncate`); only `recover` changed sides.

The block itself is optional too: an assembly document with no `middlewares`
key now gets the default chain, where it used to get an empty one. So the
recover net and the non-text part budget are in place whether the document
mentions them or not.

Opting out of `recover` turns off the reporting half, not the safety net:
`tool.Executor.Execute` still converts a panic escaping the chain into an
`IsError` result. A document that sets `enabled: false` loses the tool-name
attribution, the warning and the stack — not the process's survival.

In Go, `middleware.RecoverSettings.Enabled` is now `*bool`, so "not
mentioned" and "explicitly false" are distinguishable. A literal that
compiled before needs a pointer:

```go
// before
middleware.Settings{Recover: &middleware.RecoverSettings{Enabled: true}}
// after
enabled := true
middleware.Settings{Recover: &middleware.RecoverSettings{Enabled: &enabled}}
```

Recovery also became a contract of the fan-out boundaries rather than a
middleware's private job, because middleware cannot reach all of them:

- `tool.Executor.Execute` converts a panic escaping the chain into an
  `IsError` result, so an executor assembled without `Recover` — in a test, in
  a host with its own chain — still cannot take the process down.
- A panic in a parallel graph branch fails that branch and the wave instead of
  the process.
- A panic in `runtime/session`'s `Sink.OnDelta` is reported as the delivery
  failure it is, and the sink detaches.

## `concurrency.limit` must be positive

A `concurrency` block with `limit: 0` used to be ignored; it is now a document
error, matching `result_limit.max`. Omit the block to run without the limiter:

```yaml
middlewares:
  concurrency: {limit: 8}   # limit must be > 0 when the block is present
```

## The keychain store is one handle per directory

`core/secret/keychain` used to open a separate backend per `NewStore` call, so
the deployment resource and the app-side `keychain.Manager` — the two handles
the deployment guide tells a host to use together — each held their own lock
over the same `accounts.json`. Two concurrent `Set` calls could interleave
their read-modify-write and drop an index entry, leaving a sealed file nothing
could enumerate.

Every store opened over one directory in a process now shares one backend and
one lock, and `NewStore` resolves its directory to an absolute path with
symlinks resolved first. A spelling that resolves to a directory already being
served joins that store too — compared by file identity, so the two cases of
one name on a case-insensitive volume are one store, not two locks over one
set of files.

Opening a directory that already has a store re-reads its key file rather than
keeping the key the process remembers: the key on disk is the source of truth,
and a store that was reset, restored from a backup or re-keyed under a running
process must write files the next process can open. The adopted key waits for
the operations already running under the old one, so a `Set` cannot seal under
one key and publish after the swap. Cross-process writers are still not
serialized: management remains an app-side, single-writer surface.

## Reload applies session limits as part of the commit

`Runtime.Reload` now applies the new `sessions` tunables in the commit, just
before the epoch swap, instead of while draining the agents the document
removed. A reload that aborts leaves the serving generation under its own
limits, and the values are validated in the phase that only reads the
document, so a bad `sessions` block fails before anything is drained.

A drain itself is still not reversible — closing those sessions is what
draining means — so an abort after a successful drain reopens the agent names
but not the conversations. That was always true; it is now reported: a
warning per drained agent, and `drained_agents` on the
`runtime.rebuild.failed` event. A drain that fails while closing sessions
still counts as drained, because those conversations are gone either way.

## Delegation run ids are keyed by the delegation id

`delegation.RunIDNotifier` lets an async backend learn the subagent's run id as
soon as the turn exists — before there is a terminal response, and before
`delegation_status` has anything to report. It was keyed by the stream escrow
ref, which a delegation only has when it inherits a sink: a delegation that
inherited none had its note dropped and stayed without a run id, even though
the turn was running.

`NoteRunID` now takes the delegation id — the id `Submit` returns, and the one
`Work.ID`, `Response.ID` and `delegation_status` carry. The signature is
unchanged, which is the trap: an implementation that still reads the argument
as an escrow ref compiles and then no-ops silently, because every id it is
handed is a delegation id it cannot find. `kanban.Board.NoteRunID` is the
reference — it looks the card up by id rather than scanning for a matching
ref.

A queue-owned worker that drives `Runner.Run` directly supplies the claimed id
through the new `delegation.WithWorkID`; the stamp is consumed at the run
boundary, so a delegation started inside the subagent cannot report its run id
against the parent's work item.

Terminal responses carry the run id too, under `delegation.RunIDMetadataKey`
(`delegation.run_id`), beside the session identity under
`delegation.SessionIDMetadataKey`. Async callers need it there: `Response.ID`
holds the delegation id, so this key is the only place the run stays
addressable. It reports an id the run actually reported — a turn canceled
before it started leaves the key absent — so read it as evidence, not as a
promise.

## `InputTokenUsage.ValidateFor`

`usage.InputTokenUsage.Validate` became `ValidateFor(inputTokens int64)`,
mirroring `OutputTokenUsage.ValidateFor`. The new argument lets the per-modality
breakdown be checked against its total: modality tokens are a subset of the
prompt, so their sum may under-report (drivers attribute only the non-text
parts they are told about) but must never exceed it. A zero total counts as
no total: a driver that has a breakdown but no total to compare it with (an
audio response where only the audio detail is populated) is accepted, since
the field cannot tell "the provider reported zero" from "the provider
reported nothing".

## Error wrapping

`errors.New(err.Error())` severs the chain — nothing `errors.Is` or
`errors.As` could match survives it — and it is now gated by
`core/errdefs`. The classified constructors (`Validationf`, `Timeoutf`, …)
stay a review convention, as the package doc says: `errorlint` cannot see
inside them.

## Checkpoints are incremental

A checkpoint used to be a full board snapshot, taken at every boundary an engine
recognised: each graph wave, each `a2a` task transition, each runtime session
turn. That cost `O(history)` per boundary — one deep copy plus one JSON encode
of the whole board — so a long session grew quadratically, and a host without a
checkpoint store paid it too, because the snapshot was an argument to a call
that then did nothing.

Three things change, and none of them change what a caller reads:

- **`Board` records its writes.** `Board.Mark()` and `Board.Since(mark)` hand a
  consumer the writes after a position instead of a copy of the state, and
  `BoardDelta` is their value type. This is the basis of every saving below.
  It comes with one contract: **a value stored in a var is immutable once
  written**. Change it with `SetVar`, not by mutating what `Var` returned. That
  mutation was already a data race against any concurrent snapshot or parallel
  branch; it would now also be a gap in the record. `agent.AuditBoardValues(true)`
  makes `Var`/`Vars` return copies so a test can catch it. Empty `core/agent` and
  `core/graph` run their suites with the audit on.
- **A store may take increments.** `CheckpointJournal` adds `WriteBase` and
  `Append`; `Save` on a journalling store is a base write and discards the chain.
  `Load` keeps its signature and returns the rebuilt record, so a reader never
  learns whether a record cost one row or a hundred. `backends/checkpoint/sqlite`
  stores increments in a second table and replays them on load; a gap is
  reported as `errdefs.NotAvailable` (the host's cue to start a fresh run)
  rather than silently returning partial state.
- **Engines ask before they build.** `CheckpointTarget` is an optional Host
  capability reporting whether the host persists anything and whether it takes
  increments. A host that does not declare it is assumed to persist full
  records — the previous behaviour — so nothing that exists today has to change.
  `runtime.baseHost` answers from its configured store, which is what stops a
  deployment without one from copying the board at every wave.

Adoption is a deployment knob and a store contract:

| Surface | Before | After |
| --- | --- | --- |
| `deploy.yaml` graph build settings | — | `build.checkpoint_compaction: <waves>` (optional, default `32`) |
| A custom `agent.CheckpointStore` | `Save`/`Load` | unchanged; add `WriteBase`/`Append` to take increments |
| `Host.Checkpoint` | called by every engine | still called, but through `agent.CheckpointWriter` |
| A custom `Host` (`HostFuncs`) | — | unchanged; declare `CheckpointCapabilities` to opt into the cheap paths |

Two behaviours change for a deployment that reconfigured nothing, both on the
error path:

- Parallel branch results carry `BranchResult.Delta`; `Snapshot` is populated
  only when a custom `ParallelConfig.Merge` is installed, because a custom merge
  was written against absolute state. The built-in strategies merge increments,
  so a wave no longer holds one board copy per branch.
- A node retry no longer takes a board snapshot per attempt: one rollback point
  per node, and none at all with the default `max_node_retries: 0`.

The guarantees are unchanged: a checkpoint is best-effort, a store failure never
fails a run, and the next boundary retries from the position the failed one did
not advance. See [Checkpoints](../guides/checkpoint.md) for the store contract
and the compaction knob.
