---
layout: default
title: Tool System
---
# Tool System Guide

`core/tool` defines the LLM function-calling contract, a registry/catalog
directory, the executor dispatcher, and a middleware chain.

## Roles

| Type | Responsibility |
| --- | --- |
| `Registry` | mutable, thread-safe directory of tools |
| `Catalog` | read-side view (`Get`, `Definitions`) |
| `Executor` | dispatches every call through middleware |

`Registry.Add` / `Registry.Remove` register and unregister tools at
runtime (removal closes `io.Closer` tools); deferred sources use this
surface to publish tools discovered after construction.

## Tool contract

```go
type Tool interface {
    Definition() message.ToolDefinition
    Execute(ctx context.Context, arguments string) (string, error)
}
```

`Definition` carries `Name`, `Description`, and `InputSchema`. `arguments`
is a JSON-encoded object.

## Deployment resources

Tool sources feed an assembly:

```yaml
resources:
  sim:
    kind: tool.Source
    impl: sim
  tools:
    kind: tool.Assembly
    impl: memory
    deps:
      tool: sim
```

Dynamic injection is configured in the assembly settings:

```yaml
resources:
  tools:
    kind: tool.Assembly
    impl: memory
    deps:
      tool: sim
    settings:
      dynamic:
        default: deferred
        exposures:
          tool_search: always
        budget:                   # per-round visible set
          max_definitions: 32
          max_bytes: 16384
        discovery:                # persistent discovery pool
          max_tools: 32
          max_bytes: 16384        # independent byte cap
          idle_rounds: 10
```

`tool_search` is the discovery tool: a query plus an optional limit.
Matching tools are loaded and added to the session's discovery pool
automatically (no `select` step), and their real schemas become visible
from the next round. Pool entries stay visible while they are used:
every executed call refreshes the entry, idle entries are evicted after
`discovery.idle_rounds`, and the pool never exceeds
`discovery.max_tools` / `discovery.max_bytes` — the per-round
`budget` still caps what is actually sent to the model each turn.
`discovery.max_bytes` is independent of `budget.max_bytes`; raising it
above the per-round budget keeps lower-priority entries as a loaded
cache that costs no tokens; they return to the visible set when used or
re-searched.
`tool_search` must stay `always`; a different exposure is rejected at
assembly build time. The legacy `selected_retention` and `recent_window`
policy keys are deprecated and only seed `discovery.idle_rounds` when it
is unset.

MCP servers are registered as a `tool.Source/mcp` resource. Attach is
best-effort: a server that is unreachable at startup is retried in the
background with exponential backoff, and its tools are published to the
registry the moment it connects. A server that dies later is
reconnected the same way; its tools stay registered and calls fail with
a per-server `NotAvailable` error until the connection is restored.
Configuration errors (a rejected connection, an invalid spec) still
fail the deployment. The settings subtree declares one or more servers:

```yaml
resources:
  sim:
    kind: tool.Source
    impl: mcp
    settings:
      servers:
        - name: filesystem
          transport: stdio           # stdio | http
          command: npx
          args: ["-y", "@modelcontextprotocol/server-filesystem"]
          env: {TOKEN: ${env:MCP_TOKEN}}
          prefix: fs                  # tool namespace; default "<name>__"
          resources: true             # bridge list_resources / read_resource tools
          required: true              # host should WaitReady before serving
        - name: remote
          transport: http
          url: https://mcp.example.com/mcp
          headers: {Authorization: "Bearer ${env:MCP_TOKEN}"}
          http_timeout: 30s
```

`required: true` marks a server the host cannot start without; hosts
await `Source.WaitReady` so a background give-up surfaces as an error
instead of a silent missing tool set. Middleware lives in
`core/tool/middleware`.

## Middleware chain

`tool.Assembly/middleware` is the memory assembly plus a
settings-declared middleware chain:

```yaml
resources:
  tools:
    kind: tool.Assembly
    impl: middleware
    deps:
      tool: sim
    settings:
      middlewares:
        recover: {enabled: true}
        telemetry: {enabled: true}
        result_limit: {max: 20000}
        timeout: {default: 30s}
        concurrency: {limit: 8}
```

Each entry is optional; absent entries are skipped. `recover` converts a
panicking tool (or inner middleware) into an `IsError` result instead of
crashing the caller's goroutine; `telemetry.enabled` records an
OpenTelemetry span, executions/duration/error metrics, and a warning log
for each call; `timeout.default` bounds each call with a Go duration (calls
that already carry a deadline pass through); `concurrency.limit` caps
in-flight executions, with excess callers waiting (respecting context
cancellation); `result_limit.max` caps the text of one result in runes and
`result_limit.part_budget_bytes` caps the encoded size of its non-text parts
(images, audio, file references, structured data) — absent means 1 MiB, `0`
lifts the cap. Whatever exceeds a budget is dropped and the truncation
marker (`result_limit.marker`, default `…[result truncated]`) is appended, so
the model learns the result was shortened, and text never exceeds
`result_limit.max` runes once anything was cut: the marker's runes are
reserved rather than added on top.

Non-text parts are bounded by default, even without a `result_limit`: a tool
result rides every later turn's context, and an inline image or audio payload
has no natural size. `result_part_budget_bytes` at the `middlewares` level
moves that default (absent means 1 MiB, `0` lifts it); when `result_limit`
declares its own `part_budget_bytes`, that value is the one that applies. The
plain `memory` impl runs no middleware at all, so a deployment that uses it
bounds tool results in its own tools.

A model that calls a deferred tool before `tool_search` has exposed it is
rejected at response validation with a distinguishable `undefined_tool`
error rather than the generic `invalid_provider_response`. The inference
node's `undefined_tool_recovery` config turns that rejection into board
feedback that sends the model back to `tool_search` on the next round,
without writing an engine-generated turn into the conversation transcript;
see [graph.md](graph.md) for the recovery loop.

See [runtime.md](runtime.md) for per-session dynamic catalogs.
