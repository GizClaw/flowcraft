---
layout: default
title: Tool System
---
# Tool System Guide

`sdk/tool` is the LLM function-calling contract: the `Tool` interface,
a `Registry`/`Catalog` directory, the `Executor` dispatcher, and a
middleware chain for cross-cutting policy. Built-in tool adapters
ship in `sdkx/tool`; the MCP bridge turns external servers into
catalog entries.

The package is intentionally split. Every cross-cutting execution
policy — recovery, telemetry, concurrency, timeout, rate limit,
approval, audit — is **middleware**, declared once at `Executor`
construction and applied uniformly to every call.

## Concepts

### Registry vs Catalog vs Executor

Three roles, deliberately separated:

| Type       | Responsibility                                                                                                                                       |
| ---------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Registry` | "what tools exist" + scope metadata; the mutable, thread-safe directory                                                                              |
| `Catalog`  | read-side view (`Get`, `Definitions`); a `Registry` implements it; filtered views and remote proxies can implement it without owning dispatch policy |
| `Executor` | "how a call runs"; applies the middleware chain to every call and dispatches through the `Dispatch` contract                                         |

A `Registry` is a `Catalog`. An `Executor` is constructed against a
`Catalog` and uses `Registry` only to look tools up by name. Tests
and adapters may substitute their own `Dispatch` instead of using
`*Executor` (see `Dispatcher` interface).

### The Tool contract

```go
type Tool interface {
    Definition() Definition
    Execute(ctx context.Context, arguments string) (string, error)
}
```

`arguments` is a JSON-encoded object matching the tool's declared
`InputSchema`. `Execute` returns the JSON-encoded result string
(messages persist it as a `role=tool` content); returning an error
becomes `Result{IsError: true, Content: err.Error()}` inside the
middleware chain, not an out-of-band error.

`ToolMeta` is optional and advisory — a zero value means "no claims,
treat conservatively":

| Field          | Drives                                                                     |
| -------------- | -------------------------------------------------------------------------- |
| `RateLimit`    | request-per-second throttling middleware                                   |
| `MutatesState` | gates whether retry-on-failure logic may re-invoke with the same arguments |
| `SelfTimeout`  | opts out of the timeout middleware's default deadline                      |

### Middleware

A `Middleware` is a function that wraps a `Dispatch`:

```go
type Middleware func(next Dispatch) Dispatch
type Dispatch  func(ctx context.Context, call Call) Result
```

Middlewares compose in **outermost-first** order: the first registered
middleware sees the call first and the result last. A middleware
MUST forward to `next` unless it intentionally short-circuits (e.g.
policy denial); short-circuits set `Result.IsError=true` and put a
human-readable reason in `Content`.

## First tool

```go
package main

import (
    "context"
    "encoding/json"
    "fmt"

    "github.com/GizClaw/flowcraft/sdk/errdefs"
    "github.com/GizClaw/flowcraft/sdk/tool"
)

type echoArgs struct {
    Text string `json:"text" description:"what to echo back"`
}

type echoTool struct{}

func (echoTool) Definition() tool.Definition {
    return tool.Definition{
        Name:        "echo",
        Description: "Returns the input text unchanged.",
        InputSchema: tool.ObjectSchema(
            tool.Property("text", "string", "what to echo back"),
        ),
        InputSchemaJSON: tool.MustJSONSchema(echoArgs{}),
    }
}

func (echoTool) Execute(ctx context.Context, args string) (string, error) {
    var a echoArgs
    if err := json.Unmarshal([]byte(args), &a); err != nil {
        return "", fmt.Errorf("echo: parse args: %w", err)
    }
    return a.Text, nil
}

func main() {
    reg := tool.NewRegistry()
    _ = reg.Register(echoTool{}, tool.ScopeAgent)

    exe := tool.NewExecutor(reg /*, middlewares... */)

    out, _ := exe.Execute(context.Background(), tool.Call{
        Name:      "echo",
        Arguments: `{"text":"hi"}`,
    })
    fmt.Println(out.Content) // "hi"
}
```

## Defining the input schema

Two equivalent forms ship in the box:

```go
// Programmatic form — minimal, type-checked.
def := tool.Definition{
    Name:        "search",
    Description: "Search the web.",
    InputSchema: tool.ObjectSchema(
        tool.Property("query", "string", "search query"),
        tool.ArrayProperty("tags", tool.Items("string"), "optional filter"),
        tool.Required("query"),
    ),
}
```

```go
// JSON-Schema form — use when you already have a JSON Schema
// (e.g. an MCP tool definition) and want exact parity.
def := tool.Definition{
    Name:            "search",
    Description:     "Search the web.",
    InputSchemaJSON: tool.MustJSONSchema(searchArgs{}),
}
```

Both produce a definition the LLM sees identically. Pick the form
that matches how the schema arrives — programmatic for hand-rolled
tools, JSON for reused schemas.

## Tool scopes

`Registry` carries scope metadata that controls `tool_list`
visibility without changing the definition:

| Scope      | Constant             | Visibility                                                                                                    |
| ---------- | -------------------- | ------------------------------------------------------------------------------------------------------------- |
| `agent`    | `tool.ScopeAgent`    | visible in `tool_list` and to the ToolSelector (default)                                                      |
| `platform` | `tool.ScopePlatform` | hidden from `tool_list`/ToolSelector, but still addressable by exact name in an inference node's `tool_names` |

```go
_ = reg.Register(execTool{}, tool.ScopePlatform)   // exec is platform
_ = reg.Register(searchTool{}, tool.ScopeAgent)    // search is agent
```

Scope is registry-level metadata and does **not** appear in
`Definition`. An agent's `tools: [search, fetch]` allow-list is
enforced at runtime by the engine regardless of scope.

## Middleware chain

The production chain, in recommended order, ships in
`sdk/tool/middleware`:

```go
import "github.com/GizClaw/flowcraft/sdk/tool/middleware"

exe := tool.NewExecutor(reg,
    middleware.Recover(),                         // panics become IsError results
    middleware.Telemetry(),                       // spans, metrics, logs
    middleware.Concurrency(10),                   // fan-out cap
    middleware.Timeout(30*time.Second, map[string]time.Duration{
        "exec": 2 * time.Minute,                  // per-tool override; 0 exempts
    }),
    middleware.RateLimit(reg),                    // honors ToolMeta.RateLimit
    middleware.Approval(approver, "exec", "kanban_submit"),
    middleware.Audit(sink),
)
```

Every middleware is independently unit-testable, safe for concurrent
use, and composes in any order. Application-specific policies
(budgets, tenancy, secret resolution) follow the same `Middleware`
shape and slot anywhere into the chain.

Retry is deliberately not a built-in: re-invoking a tool that mutates
state is unsafe, and `ToolMeta.MutatesState` exists so a future
retry policy can make that distinction deliberately.

## Built-in tool adapters

`sdkx/tool` ships a small, deliberately curated set. Every other
capability is delegated to MCP. The rule:

> A built-in tool stays built-in only when it reaches something a
> separate process cannot: the in-process `agent.Host`, the sandbox
> boundary, or live orchestration state.

| Package                 | Tool name(s)                    | Category                                                       |
| ----------------------- | ------------------------------- | -------------------------------------------------------------- |
| `sdkx/tool/askuser`     | `ask_user`                      | host bridge (reaches the `agent.UserPrompter` host capability) |
| `sdkx/tool/exec`        | `exec`                          | sandbox boundary (reaches `sandbox.Runner`)                    |
| `sdkx/tool/kanban`      | `kanban_submit`, `task_context` | orchestration state                                            |
| `sdkx/tool/mcp`         | (catalog from MCP servers)      | adapter                                                        |
| `sdk/agent.HandoffTool` | handoff                         | agent-layer carve-out (composes `agent.Handoff` directly)      |

Per-tool schema, secrets, and usage live in each package's `doc.go`.

## Deploy integration

`tool.Assembly` is a first-party resource in deployment documents.
The YAML document describes the middleware chain and the tool
factories; the application injects the runtime collaborators
(approver, audit sink) at `Build` time:

```yaml
resources:
  tools:
    kind: tool.Assembly
    impl: yaml
    settings:
      file: ./tools.yaml
```

```yaml
# tools.yaml
version: v1
factories:
  - kind: exec
  - kind: askuser
  - kind: kanban
middlewares:
  - kind: recover
  - kind: telemetry
  - kind: concurrency
    spec: { limit: 10 }
  - kind: timeout
    spec: { default: 30s, per_tool: { exec: 120s } }
  - kind: approval
    spec: { tools: [exec, kanban_submit] }
scopes: { exec: platform }
```

Built-in middleware kinds map 1:1 to the constructors in
`sdk/tool/middleware`. Custom kinds register on the `Builder` via
`RegisterFactory` (the same hook surface `sdkx/inference/config`
uses for provider drivers).

A graph engine binds a `tool.Assembly` through its `tools` dep and
the executor becomes the dispatch target for `tool` nodes and for
inference-driven tool calls.

## Testing

Two complementary patterns:

**Tool-level unit test** — call `Execute` directly with a JSON
argument string. This is the right level for testing argument
parsing, error wrapping, and result shape.

**Middleware unit test** — build a `Dispatch` stub returning a
canned result, wrap it in the middleware under test, and assert on
the produced `Result` and the stub's call count. No registry needed.

**End-to-end** — the package ships `sdk/tool/tooltest` for the
shared black-box contracts (executor, registry, middleware).
Production middleware live alongside the same suite.

For tools wired into a deployment, the test is "parse a `tools.yaml`
fixture, build the assembly, dispatch a call, assert on the result" —
the same shape as the [deploy guide's testing section](deploy.md#testing-your-deployment).

## Further reading

- Primitive contract: `sdk/tool/tool.go`, `sdk/tool/registry.go`,
  `sdk/tool/executor.go`, `sdk/tool/middleware.go`.
- Built-in middleware: `sdk/tool/middleware/doc.go` (per-middleware
  semantics), `sdk/tool/tooltest` (shared test contracts).
- Built-in tools: `sdkx/tool/doc.go` (the curation rule),
  `sdkx/tool/{exec,askuser,kanban,mcp}/doc.go`.
- Assembly: `sdkx/tool/config/doc.go`, the `tool.Assembly` resource
  in [deploy.md](deploy.md#first-party-impls).
