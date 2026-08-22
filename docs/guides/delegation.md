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

See [runtime.md](runtime.md) for `WithResultHostFactory` and reload, and
[tool.md](tool.md) for tool sources.

## Sources of truth

`core/delegation/delegation.go`, `core/delegation/service.go`,
`core/delegation/directory.go`, `core/delegation/host.go`,
`core/delegation/hostwrap/`, `core/delegation/resource.go`,
`core/delegation/tool/`.
