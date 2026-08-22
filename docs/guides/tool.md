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
```

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
        timeout: {default: 30s}
        concurrency: {limit: 8}
```

Each entry is optional; absent entries are skipped. `recover` converts a
panicking tool (or inner middleware) into an `IsError` result instead of
crashing the caller's goroutine; `timeout.default` bounds each call with a
Go duration (calls that already carry a deadline pass through);
`concurrency.limit` caps in-flight executions, with excess callers waiting
(respecting context cancellation). The plain `memory` impl rejects the
`middlewares` key.

A model that calls a deferred tool before `tool_search` has exposed it is
rejected at response validation with a distinguishable `undefined_tool`
error rather than the generic `invalid_provider_response`. The inference
node's `undefined_tool_recovery` config turns that rejection into
in-conversation feedback that sends the model back to `tool_search`; see
[graph.md](graph.md) for the recovery loop.

See [runtime.md](runtime.md) for per-session dynamic catalogs.
