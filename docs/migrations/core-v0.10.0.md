---
layout: default
title: Migrating to core/v0.10.0
---
# Migrating to `core/v0.10.0`

`core/v0.10.0` is the follow-up to the v0.9.0 review. Most of it is about
what happens *after* something has gone wrong — a panicking tool, an aborted
reload, a credential store that lost an index entry — so a host that never
hits those paths sees no difference. Two document entries do change meaning,
and two Go API surfaces moved.

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

In Go, `middleware.RecoverSettings.Enabled` is now `*bool`, so \"not
mentioned\" and \"explicitly false\" are distinguishable. A literal that
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
one lock, and `NewStore` resolves its directory to an absolute path first, so
two spellings of one directory are one store. Cross-process writers are still
not serialized: management remains an app-side, single-writer surface.

## Reload applies session limits with the swap

`Runtime.Reload` now applies the new `sessions` tunables after the epoch swap,
not while draining the agents the document removed. A reload that aborts
leaves the serving generation under its own limits.

A drain itself is still not reversible — closing those sessions is what
draining means — so an abort after a successful drain reopens the agent names
but not the conversations. That was always true; it is now reported: a
warning per drained agent, and `drained_agents` on the
`runtime.rebuild.failed` event.

## `InputTokenUsage.ValidateFor`

`usage.InputTokenUsage.Validate` became `ValidateFor(inputTokens int64)`,
mirroring `OutputTokenUsage.ValidateFor`. The new argument lets the per-modality
breakdown be checked against its total: modality tokens are a subset of the
prompt, so their sum may under-report (drivers attribute only the non-text
parts they are told about) but must never exceed it.

## Error wrapping

`errors.New(err.Error())` severs the chain — nothing `errors.Is` or
`errors.As` could match survives it — and it is now gated by
`core/errdefs`. The classified constructors (`Validationf`, `Timeoutf`, …)
stay a review convention, as the package doc says: `errorlint` cannot see
inside them.
