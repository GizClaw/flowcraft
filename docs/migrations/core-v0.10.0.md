---
layout: default
title: Migrating to core/v0.10.0
---
# Migrating to `core/v0.10.0`

`core/v0.10.0` is the follow-up to the v0.9.0 review. Most of it is about
what happens *after* something has gone wrong — a panicking tool, an aborted
reload, a credential store that lost an index entry — so a host that never
hits those paths sees no difference. Two document entries do change meaning,
two Go API surfaces moved, and one interface kept its shape while changing
what it is keyed by.

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
