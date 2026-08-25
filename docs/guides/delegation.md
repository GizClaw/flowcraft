---
layout: default
title: Delegation
---
# Delegation Guide

`core/delegation` defines backend-neutral contracts for assigning work to
another agent or execution target. `Directory` owns discovery; `Service`
owns execution and status lookup. Delegation is synchronous or
asynchronous, and the local implementation executes delegated work
through the same session lifecycle as a user turn.

## Deployment

The three core resources:

```yaml
resources:
  dir:
    kind: delegation.Directory
    impl: local    # no settings; binds the deployment's agents at wire time

  svc:
    kind: delegation.Service
    impl: local
    deps:
      directory: dir
      # backend: async            # optional; absent = sync-only service
      # session_provider: prov    # optional identity policy
    settings:
      max_concurrency: 4          # positive; default 4
      max_depth: 8                # positive; default 8
      timeout: 5m                 # Go duration; zero leaves the caller's context
      idempotency_retention: 24h  # positive; how long responses stay replayable
      defer_workers: true         # start async workers on Start instead of at build

  prov:
    kind: delegation.SessionProvider
    impl: random   # no settings; fresh ContextID per delegation, never persists
```

The directory is required and is bound to the assembled deployment during
the deploy wire phase, so every generation delegates against its own
agents. The backend and session provider deps are optional: without a
backend the service is sync-only, and without a session provider each
delegation mints a fresh `ContextID`.

## Exposing delegation to agents

Turn hosts expose the service through
`runtime.Builder.WithResultHostFactory` plus `delegation/hostwrap`; the
service is then available to execution-time consumers via
`ServiceFromHost`. The model-facing delegation tools come from a
`tool.Source` resource:

```yaml
resources:
  dtools:
    kind: tool.Source
    impl: delegation
    deps:
      directory: dir   # no settings; delegate / delegation_status / delegation_targets
```

The tools discover their targets from the same generation-bound directory
on every call, so each generation's tools see that generation's agents.

## Lifecycle

- Sync mode waits for the delegated work and returns its terminal
  response; async mode admits work and returns immediately, with status
  lookup through `delegation_status`.
- Successful responses stay replayable for `idempotency_retention`, so
  retried delegation does not re-execute completed work.
- A `Reload` re-binds the directory and service to the new generation,
  so in-flight delegation completes on the generation it started on.

## Session identity and resume

When a session manager is bound, delegated work runs through the runtime
session lifecycle. `SessionProvider.Persistent()` is provider-wide: a
persistent provider's `ContextID` is stable, durable, and resumable, while
providers that mint a fresh `ContextID` per delegation (e.g. `random`) are
ephemeral and never write state. A one-shot target behind a persistent
provider can therefore still opt out by returning a fresh `ContextID`.

A retried delegation with the same `ContextID` resumes the parked run from
its last checkpoint instead of starting fresh, but only when **both** the
request and the session key match:

- identical message and metadata inputs → the turn replays from the
  checkpoint under the original run id (`runtime.sessions.resume` and a
  `checkpoint_store` are required);
- a different message or inputs → a fresh turn starts on the same session;
- any failure to replay — a missing checkpoint, unreadable parked state,
  transient checkpoint-store read errors, or an engine that cannot
  resume — falls back to a fresh start with a warning rather than
  failing the retry.

Resume replays the request stored with the parked run; callers retrying a
job should send the exact same message and inputs. After a successful run
the parked marker is cleared, so a later delegation of the same key starts
fresh.

Agent preparers run again on resume before the engine restores the
checkpoint board. Preparers with side effects (uploads, live-progress
resets) must be idempotent or detect the replay via `agent.ResumeContext`
/ `Run.ResumeFrom` and skip.

## Stream inheritance

Delegated subagent turns inherit the caller turn's stream sinks: the turn
execution context carries the caller's stream policy, and the delegation
service attaches those sinks to the subagent session as observers.
Authority and explicit-ack semantics are downgraded on inheritance — the
subagent turn is never handed to the inherited sink, so it cannot fulfil
authoritative/explicit-ack obligations (an unacked window would otherwise
detach the attachment mid-run). Visibility is preserved.

An inherited sink may be invoked concurrently from multiple sessions
(the caller turn plus parallel subagents) and must be safe for concurrent
`OnDelta` calls.

Async delegation inherits the same way, across the queue boundary:

- **In-process (escrow):** the submit side stores the caller's live sink
  specs in a service-side escrow referenced by `AsyncRequest.Stream.Ref`;
  the worker restores them and attaches them, so deltas reach the exact
  sink instances the caller's UI is listening on. Entries are released
  at terminal completion and swept by TTL as a backstop.
- **Cross-process (target + resolver):** the submit side also persists a
  serializable `StreamTarget` (`AsyncRequest.Stream.Target`) describing
  the destination. When no escrow entry survives (TTL expiry, restart, a
  worker in another process), the worker resolves the target through the
  runtime's whitelisted `StreamTargetResolver`
  (`runtime.StreamExportRegistry`).
- **Reachability:** `conversation` targets resolve to a live sink
  registered in the resolving process's registry — they recover streams
  in-process but do not deliver across processes. `bus` targets forward
  onto a named event bus and are the kind capable of true cross-process
  delivery, as long as the bus transport spans the processes.
- **Single-destination caveat:** `StreamRef.Target` is single-valued.
  The in-process escrow preserves every inherited sink; the
  cross-process path restores exactly one. The exporter prefers
  broadcast (bus) targets when several sinks are describable. Sinks
  describe themselves by implementing `delegation.StreamTargetProvider`,
  so UI decorators can pass the description through without breaking
  recognition.
- **Lifecycle:** async stream deltas carry lineage headers (`run_id`,
  `parent_run_id`, `tool_call_id`, `agent_id`) but the sink sees no
  explicit EOF; terminal state is reported through kanban card events
  and `delegation_status`.

See [runtime.md](runtime.md) for `WithResultHostFactory` and reload, and
[tool.md](tool.md) for tool sources.

## Sources of truth

`core/delegation/delegation.go`, `core/delegation/service.go`,
`core/delegation/directory.go`, `core/delegation/host.go`,
`core/delegation/hostwrap/`, `core/delegation/resource.go`,
`core/delegation/tool/`.
