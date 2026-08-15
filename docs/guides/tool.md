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
fail the deployment. Middleware lives in `core/tool/middleware`.

See [runtime.md](runtime.md) for per-session dynamic catalogs.
