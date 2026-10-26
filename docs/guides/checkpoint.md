---
layout: default
title: Checkpoints and Resumable State
---
# Checkpoints and Resumable State

`core/agent` owns the checkpoint contract; every engine decides when a boundary
is safe and every host decides whether anything is stored. This page covers both
sides: what an engine persists and what it costs, and how to implement a store
that survives a long session.

- [Why a checkpoint is not a board copy](#why-a-checkpoint-is-not-a-board-copy)
- [The write record](#the-write-record)
- [What an engine persists](#what-an-engine-persists)
- [Capability negotiation](#capability-negotiation)
- [Compaction](#compaction)
- [Writing a store](#writing-a-store)
- [Failure semantics](#failure-semantics)

## Why a checkpoint is not a board copy

A long session grows monotonically: every turn appends messages and writes vars,
and the board keeps all of it because a resume needs all of it. The naive
implementation — snapshot the board, store the snapshot — therefore costs
`O(history)` per boundary, and a run with `N` boundaries costs `O(N × history)`.

The board is append-heavy, so almost nothing changes between two boundaries. A
checkpoint is a **base record plus a chain of increments**, and the engine only
pays for the writes that happened since the last one:

| boundary | what is written |
| --- | --- |
| first wave of a run, every compaction window, a resume | base: the whole board |
| every other wave | increment: the vars written and messages appended since the last one |

`Load` hides the difference: it returns the same record it always did, rebuilt
from the base and its chain.

## The write record

`Board.Mark()` records a position; `Board.Since(mark)` returns what changed after
it. Both are cheap — a mark is a counter plus one length per channel, and a delta
is proportional to the writes, not to the state:

```go
mark := board.Mark()
// ... node handlers write vars and append messages ...
writes := board.Since(mark)   // only this window's writes
```

This is why the board owns an **ownership contract**: a value stored in a var is
immutable once written, and a change goes through `Board.SetVar` (or
`DeleteVar`/`AppendSliceVar`/`UpdateSliceVarItem`/`SetChannel`). The write record
is what makes an increment possible, and only the board's own methods are
recorded. Mutating a value read back from `Board.Var` is already a data race
against any concurrent snapshot or parallel branch — now it would also be a
silent gap in the record. `agent.AuditBoardValues(true)` makes `Var`/`Vars`
return copies, so a test can catch the violation where it is introduced.

Channels are append-only by convention; a channel that is set wholesale or
popped is recorded as a replacement and travels whole.

## What an engine persists

| engine | boundary | notes |
| --- | --- | --- |
| `core/graph` | every completed wave | `Steps` is the wave, so a resume rebuilds the frontier from every branch's outgoing edges |
| `core/agent/a2a` | every task state transition | the remote task id and the already-seen message ids ride in `Payload` |
| `core/runtime/session` | every turn | the committed board plus the parked run id and request, under `session-<hash>` |

All three go through `agent.CheckpointWriter`, which owns the base-versus-delta
decision. Engines do not call `Host.Checkpoint` directly: doing so builds the
record before anyone discovers that nothing will store it.

## Capability negotiation

`CheckpointTarget` is an optional Host capability, resolved through
`agent.CheckpointCapabilitiesFromHost` (which follows `HostUnwrapper`
decorators):

| host | `Enabled` | `Journal` |
| --- | --- | --- |
| `runtime.baseHost` with a store | `true` | the store, when it implements `CheckpointJournal` |
| `runtime.baseHost` without a store | `false` | — |
| `HostFuncs` with `CheckpointFn` | `true` | none: a replacement writer only promises full records |
| `HostFuncs`/`TracingMiddleware` delegating | the inner answer | routed through the decorator |
| any host that does not declare it | `true` | none |

The default is deliberately conservative: a host written before this capability
existed keeps receiving self-contained records. `Enabled: false` is an explicit
statement — "nothing is stored" — and it is what makes a deployment without a
checkpoint store stop paying for a board snapshot at every wave.

## Compaction

A chain is bounded by `build.checkpoint_compaction` (default
`agent.DefaultCheckpointCompaction`, 32): the writer appends that many increments
and then rewrites a base. The window is a **resume-latency** knob, not a memory
knob — a load replays the base plus at most that many increments. A store may
also cap its own chain and answer `agent.ErrNeedsBase`, which the writer answers
with a base; `sqlite.WithMaxDeltas(n)` is that cap.

```yaml
agents:
  assistant:
    engine:
      kind: agent.Engine
      impl: graph
      settings:
        graph: {file: ./graphs/assistant.json}
        build:
          checkpoint_compaction: 16   # full record every 16 waves
```

A writer whose store cannot take increments (`Journal == nil`) writes a full
record per boundary, exactly as before.

## Writing a store

Implement `agent.CheckpointStore` first — `Save` and `Load`, with `Load`
returning the rebuilt record — and run `agenttest.CheckpointStoreSuite`. To take
increments, add `agenttest.CheckpointJournalSuite` plus the two methods:

```go
type CheckpointJournal interface {
	// WriteBase replaces the record and discards its chain.
	WriteBase(ctx context.Context, cp Checkpoint) error
	// Append extends the record; ErrNeedsBase asks for a full one.
	Append(ctx context.Context, delta CheckpointDelta) error
}
```

`Save` must behave like `WriteBase`. Two rules make a chain trustworthy:

- **Apply in sequence order.** `CheckpointDelta.ApplyTo` folds one increment onto
  a decoded record; `CheckpointDelta.Validate` rejects a malformed one.
- **Fail closed.** A gap, or an increment with no base, means the record cannot
  be rebuilt; `Load` must return an `errdefs.NotAvailable` error so the host
  starts a fresh run instead of resuming half a conversation.

`backends/checkpoint/sqlite` is the reference implementation: a base row plus a
`(exec_id, seq)` table, replayed on load. `agenttest.JournalStore` is an
in-memory double that also reports how many bases and increments were written, so
an engine test can assert the shape of a run.

## Failure semantics

Checkpoints are best-effort: a store error never fails a run. Engines log it and
carry on — an aborted reload, a full disk or a locked database must not take the
conversation with it. What a failure does *not* do is advance the writer: the next
boundary retries from the same position, so the writes covered by the rejected
boundary are persisted by the next successful one.
