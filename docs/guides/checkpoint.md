---
layout: default
title: Checkpoints
---
# Checkpoints Guide

`core/agent` defines what a checkpoint is and what a store must do; a backend
(`core/agent/checkpoint` for SQLite, `agenttest.MemStore` for tests) does the
storing.

A checkpoint is **a task plus the deployment it ran under**:

```go
type Checkpoint struct {
    Schema     int                        // 1; a reader that does not know it refuses
    Version    int64                      // the store advances this on every save
    ServerVersion int64                   // a protocol adapter's own counter
    Fingerprint Fingerprint               // the deployment that wrote this
    Task       Task                       // the projection: what is true now
    Sections   map[string]json.RawMessage // engine-owned state
}
```

It does not contain a runtime: a runtime holds processes, handles and connections
that cannot be serialised. What can be is the fingerprint of the stack needed to
rebuild one, plus the transcript to replay — which is why a checkpoint is a
retrieval point for the whole deployment, not just the conversation.

## The store contract

```go
type Store interface {
    Save(ctx context.Context, checkpoint Checkpoint, events []EventRecord, options ...SaveOption) (first, last int64, err error)
    Load(ctx context.Context, taskID string) (Checkpoint, error)
    LatestForSession(ctx context.Context, sessionID string) (Checkpoint, error)
    Events(ctx context.Context, taskID string) ([]EventRecord, error)
    EventsAfter(ctx context.Context, taskID string, after int64) ([]EventRecord, error)
    SetServerVersion(ctx context.Context, taskID string, version int64) error
    TruncateEvents(ctx context.Context, taskID string, seq int64, kinds ...string) error
    List(ctx context.Context) ([]TaskSummary, error)
    Delete(ctx context.Context, taskID string) error
    PutBlob(ctx context.Context, data []byte) (string, error)
    // …plus the blob reader
}
```

The interface is semantic rather than relational: a task is addressed by id, and
a first save is just a save, so create and append collapse into `Save`. Where
state lives — a local file, Postgres, an object store — is a deployment decision.

| Call | Answers |
| --- | --- |
| `Save` | Writes the projection and appends this round's events in **one transaction**; returns the sequence range it assigned. |
| `Load` | The newest projection of one task. It does not replay: `Task.History` is always empty. |
| `LatestForSession` | The newest task of a session — what resuming a session needs, since checkpoints are keyed by task. |
| `Events` / `EventsAfter` | Every appended fact, or the suffix after a sequence, oldest first. |
| `SetServerVersion` | Advances the adapter's counter without rewriting the projection. |
| `TruncateEvents` | Drops events at or before a sequence, optionally only those of some kinds. |
| `PutBlob` | Stores bytes by content hash: the same bytes must yield the same reference, so a repeated write costs nothing. |

A missing task is `agent.ErrNoCheckpoint`, not an empty struct — a resume that
cannot tell "nothing yet" from "an empty task" is a resume that silently starts
over.

## Projection and event log

Both are written, in one transaction, because they answer different questions:

- the **projection** answers "what is true now" — state, round, pending tool
  call, artifacts;
- the **event log** answers "how it got there" — appended facts ordered by `Seq`,
  dated by `At`.

Storing only the projection makes every write carry the whole transcript; storing
only events makes every read a replay. Keeping `Load` projection-only is what
makes it cost the same for a one-turn task and a thousand-turn one. A reader that
needs history asks for it.

`TruncateEvents` takes kinds for the same reason: an engine that keeps trace
records beside its state compacts only the state, so the records survive the
compaction that would otherwise be indistinguishable from data loss. The newest
event is never removable, so sequence numbers stay monotonic.

## Two counters

| Counter | Owned by | Advances on |
| --- | --- | --- |
| `Version` | the store | every save; it is what a concurrent writer is compared against. |
| `ServerVersion` | a protocol adapter | only that adapter, through `SetServerVersion`. The loop's own saves never touch it. |

Two writers sharing one counter would reject each other's writes, and both are
legitimate: the engine owns the state, the adapter owns its token.

## The fingerprint

`Fingerprint` is the deployment identity a checkpoint was written under — the
runtime digest, the engine identity and the settings that define them. A resume
under a different fingerprint is not the same deployment, so a host can refuse
rather than replay a transcript into a stack that did not produce it. For the
graph engine the fingerprint is the document's **path and bytes**, which is why
editing the graph invalidates the checkpoints taken under the old one.

## Who writes what

| Writer | Boundary | Notes |
| --- | --- | --- |
| `core/agent-graph` | every step, plus a base every `checkpoint.every` steps | A base carries the whole state; the interval is the price paid for a short replay. See [graph.md](graph.md). |
| `core/agent-a2a` | every task state transition | the remote task id and the already-seen message ids ride in the payload |
| `core/agent-host` | every turn | the session table resumes a conversation with `LatestForSession` |

## Backends

```go
store, err := checkpoint.Open("state.db", checkpoint.WithSynchronous(checkpoint.SynchronousNormal))
defer store.Close()
```

`core/agent/checkpoint` is two tables plus a blob table:

```text
tasks   one row per Task: the immutable seed, the mutable projection, a version,
        and a save counter that orders a session's tasks
events  the appended facts, one row per fact, keyed (task_id, seq)
blobs   artifact bytes by content hash
```

"The newest task of a session" is the greatest save counter, never a timestamp —
a text comparison over timestamps ordered two saves in the same millisecond
wrongly, which is the kind of bug that only shows up as a lost message after a
resume. The shapes are portable (TEXT stays TEXT, BLOB becomes BYTEA) and the
only upsert uses the standard `ON CONFLICT` form, so a Postgres sibling is a
translation rather than a redesign.

`agenttest.Store(t, open)` is the conformance suite a backend runs instead of
re-deriving the promises. It covers what a backend gets wrong quietly: a seed
written twice, an event edited in place, a projection carrying history, a
truncate that eats the tail, two counters crossing, a session picking the wrong
task.

## What this replaced

The previous model checkpointed a **board**: an untyped var map plus message
channels, copied per branch and merged by reflection, with a `CheckpointWriter`
deciding base-versus-delta and a `build.checkpoint_compaction` window. It is
gone with the board. The current shape records a projection and an append-only
event log, and the only thing an engine decides is when to write a base.
