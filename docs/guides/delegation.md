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

See [runtime.md](runtime.md) for `WithResultHostFactory` and reload, and
[tool.md](tool.md) for tool sources.

## Sources of truth

`core/delegation/delegation.go`, `core/delegation/service.go`,
`core/delegation/directory.go`, `core/delegation/host.go`,
`core/delegation/hostwrap/`, `core/delegation/resource.go`,
`core/delegation/tool/`.
