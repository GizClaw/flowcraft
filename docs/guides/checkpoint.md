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
    Manifest   string                     // the authority and build it ran under
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

A driver never sees this interface. The turn hands it `agent.DriverStore` — `Save`
(with the compaction options), `Load`, `LatestForSession`, `EventsAfter` and the
two blob calls — so a driver can do what a run does and cannot list tasks, delete
one, or rewrite an adapter's counter. `agent.DriverStoreOf` is the adapter a host
applies; a store that already satisfies both (the in-memory and SQLite ones do)
can be handed over directly.

| Call                     | Answers                                                                                                               |
| ------------------------ | --------------------------------------------------------------------------------------------------------------------- |
| `Save`                   | Writes the projection and appends this round's events in **one transaction**; returns the sequence range it assigned. |
| `Load`                   | The newest projection of one task. It does not replay: the projection carries no transcript.                          |
| `LatestForSession`       | The newest task of a session — what resuming a session needs, since checkpoints are keyed by task.                    |
| `Events` / `EventsAfter` | Every appended fact, or the suffix after a sequence, oldest first.                                                    |
| `SetServerVersion`       | Advances the adapter's counter without rewriting the projection.                                                      |
| `TruncateEvents`         | Drops events at or before a sequence, optionally only those of some kinds.                                            |
| `PutBlob`                | Stores bytes by content hash: the same bytes must yield the same reference, so a repeated write costs nothing.        |

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

| Counter         | Owned by           | Advances on                                                                         |
| --------------- | ------------------ | ----------------------------------------------------------------------------------- |
| `Version`       | the store          | every save; it is what a concurrent writer is compared against.                     |
| `ServerVersion` | a protocol adapter | only that adapter, through `SetServerVersion`. The loop's own saves never touch it. |

Two writers sharing one counter would reject each other's writes, and both are
legitimate: the engine owns the state, the adapter owns its token.

## The fingerprint

`Fingerprint` is the deployment identity a checkpoint was written under: the name,
the program kind and the hook kinds of every agent in the set, hashed by
`deploy.Fingerprint`. A resume under a different fingerprint is not the same
deployment, so a host can refuse rather than replay a transcript into a stack that
did not produce it.

`Manifest` is the identity of what the conversation *held*: the authority its caller
stated and the build that ran it. It is a separate value because a deployment no
longer carries authority — two sessions of one agent may hold different grants — and
a task continued under a different authority is not the same task. A checkpoint
written before the field existed leaves it empty, and the comparison is skipped
rather than guessed at.

## Who writes what

| Writer          | Boundary                                               | Notes                                                                                                                                            |
| --------------- | ------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| `agent/graph`   | every step, plus a base every `checkpoint.every` steps | A base carries the whole state; the interval is the price paid for a short replay. See [graph.md](graph.md).                                     |
| `agent/a2a`     | one save per round trip to the peer                    | the peer's task and context ids, and the record of the call, ride in the driver's own section and events                                         |
| `agent.RunTurn` | every turn: one terminal save, on every path           | finish, panic, a consumer that stopped and a refused `BeforeTurn` all leave one; with no store, nothing is kept. See [runtime.md](runtime.md)    |
| `host.Agents`   | — (the reader)                                         | `request` resumes a conversation with `Store.LatestForSession`: a non-terminal task continues under its own id, a terminal one starts a new task |

## Durability

The store defaults to `SynchronousFull`: every save fsyncs its commit, so a power
loss cannot drop one. `WithSynchronous(checkpoint.SynchronousNormal)` drops the
per-commit fsync. Under WAL that is still durable against a process crash — the
write-ahead log is replayed on the next open — but a power loss can lose the last
commits that had not reached the disk. What makes the trade acceptable is that a
checkpoint is reconstructible: the event log plus the last base snapshot is what
replays the lost rounds forward, and a round is the unit that is either durably
there or not, so `NORMAL` never leaves a task that mentions a message the event
log does not have. What a host gives up is the guarantee, not the data it can
rebuild from — the replay is the cost, and how much of it depends on how often
the engine writes a base.

The store also keeps one connection (`SetMaxOpenConns(1)`) and applies its
pragmas per connection. That is deliberate — two connections writing at once
return `SQLITE_BUSY` even with a busy timeout — and it means every session's
saves serialise through one commit, and under `FULL` one fsync. A host with a
single conversation does not notice. A host with many concurrent sessions should
measure its own write path before choosing `SynchronousNormal`, rather than
assume the per-step checkpoint cost is bounded by the engine.

A save writes the round's events as multi-row `INSERT`s, chunked under SQLite's
variable limit, so a round costs one commit rather than one statement per fact.

## Backends

```go
store, err := checkpoint.Open("state.db", checkpoint.WithSynchronous(checkpoint.SynchronousNormal))
defer store.Close()
```

The option is what makes the line explicit: the default is `SynchronousFull`
(see [Durability](#durability)), and a host that picks `NORMAL` is saying it
would rather replay than fsync.

### Sharing a database file

A host that keeps its own tables in the same SQLite file hands the store the
handle it already has:

```go
db, err := sql.Open("sqlite", "session.db")
db.SetMaxOpenConns(1)   // required: see below
db.SetConnMaxLifetime(0) // required: see below
db.SetConnMaxIdleTime(0)

db.Exec("CREATE TABLE IF NOT EXISTS messages (…)")
store, err := checkpoint.OpenOn(db, checkpoint.WithSynchronous(checkpoint.SynchronousNormal))
// no defer store.Close(): the handle is the caller's
```

`OpenOn` prepares the same store `Open` does — the pragmas, the additive `ALTER`
tail and the schema — so a shared file costs no duplicated migration. Three
obligations come with it:

- **One connection, and one that does not expire.** The pragmas apply to the
  connection that ran them, so a pool leaves the store with `busy_timeout=0` (an
  immediate `SQLITE_BUSY` instead of a wait) and a `synchronous` mode that holds
  for one connection and not the others; a connection `database/sql` replaces
  takes them with it, which is why it must not have a lifetime or an idle time.
  `OpenOn` refuses a handle whose `db.Stats().MaxOpenConnections` is not 1 rather
  than reconfiguring a pool the caller set up — the expiry settings it does not
  expose, so those stay the caller's to keep.
- **The pragmas are handle-wide.** They land on everything else written through
  that handle: foreign keys are enforced on the caller's tables too, its commits
  take the synchronous mode, and its writes wait on the busy timeout. A failed
  `OpenOn` leaves the pragmas it already applied; they cannot be rolled back.
- **The caller closes.** `Close` on a borrowed store is a no-op; `agent.Store`
  has no `Close` at all, so it is a property of the backend.

A database that cannot reach WAL — a filesystem without the shared memory WAL
needs, or a file another writer holds in `delete` mode — is an error rather than
a silent downgrade. `:memory:` is exempt: it reports `memory`, and the mode is
moot on a database that cannot be reopened.

Sharing a file puts the three table names above in the caller's namespace, which
is a decision to make on purpose: they are not prefixed, and a host tool that
writes those rows directly is depending on a schema the module has never
promised to keep.

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
