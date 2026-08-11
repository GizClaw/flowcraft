---
layout: default
title: Tool System
---
# Tool System Guide

`sdk/tool` is the LLM function-calling contract: the `Tool` interface, a
`Registry`/`Catalog` directory, the `Executor` dispatcher, and a middleware
chain for cross-cutting policy. Built-in tool adapters ship in `sdkx/tool`;
the MCP bridge turns external servers into catalog entries.

The package is intentionally split. Every cross-cutting execution policy —
recovery, telemetry, concurrency, timeout, rate limit, approval, audit — is
**middleware**, declared once at `Executor` construction and applied
uniformly to every call.

## Concepts

### Registry vs Catalog vs Executor

Three roles, deliberately separated:

| Type       | Responsibility                                                                                                                                       |
| ---------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Registry` | "what tools exist" + scope metadata; the mutable, thread-safe directory                                                                              |
| `Catalog`  | read-side view (`Get`, `Definitions`); a `Registry` implements it; filtered views and remote proxies can implement it without owning dispatch policy |
| `Executor` | "how a call runs"; applies the middleware chain to every call and dispatches through the `Dispatch` contract                                         |

A `Registry` is a `Catalog`. An `Executor` is constructed against a `Catalog`
and uses it only to look tools up by name. Tests and adapters may substitute
their own `Dispatcher` instead of using `*Executor`.

### The Tool contract

```go
type Tool interface {
    Definition() message.Definition
    Execute(ctx context.Context, arguments string) (string, error)
}
```

`Definition` lives in `sdk/message` (the same type inference requests carry):
`Name`, `Description`, and `InputSchema` as raw JSON Schema.
`arguments` is a JSON-encoded object matching the declared schema. `Execute`
returns the JSON-encoded result string; returning an error becomes
`Result{IsError: true, Content: err.Error()}` inside the middleware chain,
not an out-of-band error.

`ToolMeta` is optional and advisory — a zero value means "no claims, treat
conservatively":

| Field          | Drives                                                                     |
| -------------- | -------------------------------------------------------------------------- |
| `RateLimit`    | request-per-second throttling middleware                                   |
| `MutatesState` | gates whether retry-on-failure logic may re-invoke with the same arguments |
| `SelfTimeout`  | opts out of the timeout middleware's default deadline                      |

### Middleware

A `Middleware` is a function that wraps a `Dispatch`:

```go
type Middleware func(next Dispatch) Dispatch
type Dispatch  func(ctx context.Context, call message.Call) message.Result
```

Middlewares compose in **outermost-first** order: the first registered
middleware sees the call first and the result last. A middleware MUST forward
to `next` unless it intentionally short-circuits (e.g. policy denial);
short-circuits set `Result.IsError=true` and put a human-readable reason in
`Content`.

## First tool

```go
package main

import (
    "context"
    "encoding/json"
    "fmt"

    "github.com/GizClaw/flowcraft/sdk/message"
    "github.com/GizClaw/flowcraft/sdk/tool"
)

type echoArgs struct {
    Text string `json:"text"`
}

type echoTool struct{}

func (echoTool) Definition() message.Definition {
    return message.DefineSchema(
        "echo",
        "Returns the input text unchanged.",
        message.ToolProperty("text", "string", "what to echo back"),
    ).Build()
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
    reg.Register(echoTool{})

    exe := tool.NewExecutor(reg /*, middlewares... */)

    result := exe.Execute(context.Background(), message.Call{
        ID:        "call-1",
        Name:      "echo",
        Arguments: json.RawMessage(`{"text":"hi"}`),
    })
    fmt.Println(result.Content) // "hi"
}
```

## Defining the input schema

The fluent builder is the programmatic form:

```go
def := message.DefineSchema(
    "search",
    "Search the web.",
    message.ToolProperty("query", "string", "search query"),
    message.ToolArrayProperty("tags", "optional filter", message.Items("string")),
).Required("query").Build()
```

If you already have a JSON Schema (e.g. from an MCP server), put it directly
in `Definition.InputSchema`:

```go
def := message.Definition{
    Name:        "search",
    Description: "Search the web.",
    InputSchema: json.RawMessage(`{
      "type": "object",
      "properties": {"query": {"type": "string"}},
      "required": ["query"]
    }`),
}
```

`Definition.Validate` requires `InputSchema` to be a JSON object, so both
forms produce a definition the LLM sees identically.

## Tool scopes

`Registry` carries scope metadata that controls `tool_list` visibility
without changing the definition:

| Scope      | Constant             | Visibility                                                                                                    |
| ---------- | -------------------- | ------------------------------------------------------------------------------------------------------------- |
| `agent`    | `tool.ScopeAgent`    | visible in `tool_list` and to the ToolSelector (default)                                                      |
| `platform` | `tool.ScopePlatform` | hidden from `tool_list`/ToolSelector, but still addressable by exact name in an inference node's `tool_names` |

```go
reg.RegisterWithScope(execTool{}, tool.ScopePlatform)   // exec is platform
reg.Register(searchTool{})                               // search is agent (default)
```

Scope is registry-level metadata and does **not** appear in `Definition`. An
agent's `tools: [search, fetch]` allow-list is enforced at runtime by the
engine regardless of scope.

## Middleware chain

The production chain, in recommended order, ships in `sdk/tool/middleware`:

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
    middleware.Approval(approver, "exec", "delegate"),
    middleware.Audit(sink),
)
```

Every middleware is independently unit-testable, safe for concurrent use, and
composes in any order. Application-specific policies (budgets, tenancy,
secret resolution) follow the same `Middleware` shape and slot anywhere into
the chain.

Retry is deliberately not a built-in: re-invoking a tool that mutates state is
unsafe, and `ToolMeta.MutatesState` exists so a future retry policy can make
that distinction deliberately.

## Built-in tool adapters

`sdkx/tool` ships a small, deliberately curated set. Every other capability is
delegated to MCP. The rule:

> A built-in tool stays built-in only when it reaches something a separate
> process cannot: the in-process `agent.Host`, the sandbox boundary, or live
> orchestration state.

| Package                | Tool name(s)                     | Category                                                       |
| ---------------------- | -------------------------------- | -------------------------------------------------------------- |
| `sdkx/tool/askuser`    | `ask_user`                       | host bridge (reaches the `agent.UserPrompter` host capability) |
| `sdkx/tool/exec`       | `exec`, `exec_session`           | sandbox boundary (reaches `sandbox.Runner` / `ProcessManager`) |
| `sdkx/tool/delegation` | `delegate`, `delegation_status` | orchestration state through `sdk/delegation` contracts         |
| `sdkx/tool/mcp`        | (catalog from MCP servers)       | adapter                                                        |

Per-tool schema, secrets, and usage live in each package's `doc.go`.

`delegate` supports `sync`, `handoff`, and `async`. Its result always uses
`delegation_id`; `delegation_status` accepts only that external identifier.
Kanban cards and other backend records are operational implementation details
and are not part of the tool contract.

## Deploy integration

`tool.Assembly` is a first-party resource in deployment documents. Go tools
are registered on the config builder as builtins, runtime collaborators
(approver, audit sink) are injected through `Deps`, and the document selects
which tools, sources, scopes, and middleware kinds to enable:

```go
toolBuilder := toolconfig.NewBuilder(toolconfig.Deps{
    Approver:  approver,
    AuditSink: auditSink,
})
exec.RegisterBuiltin(toolBuilder) // sandbox-backed exec / exec_session

builder.MustRegisterResource(toolconfig.NewDeployFactory(toolBuilder))
```

```yaml
resources:
  boxes:
    kind: sandbox.Registry
    impl: yaml
    settings:
      file: ./sandboxes.yaml
  tools:
    kind: tool.Assembly
    impl: yaml
    deps:
      sandbox: boxes/coding
    settings:
      file: ./tools.yaml
```

```yaml
# tools.yaml
version: v1
sources:
  - kind: builtin
    spec:
      tools: [exec, exec_session]
middlewares:
  - kind: recover
  - kind: telemetry
  - kind: concurrency
    spec: { limit: 10 }
  - kind: timeout
    spec: { default: 30s, per_tool: { exec: 120s } }
  - kind: approval
    spec: { tools: [exec] }
scopes: { exec: platform }
```

The `builtin` source resolves hand-written Go tools from the builder's
catalog; naming a tool that was not registered fails the build. Tools may be
registered either as pre-constructed values (`RegisterBuiltin`) or as
per-build factories (`RegisterBuiltinFactory`). `exec.RegisterBuiltin` uses
the factory form: the tool is built once per assembly from the
`tool.Assembly` resource's `sandbox` dep, so the deployment document decides
which sandbox `exec` and `exec_session` run under. Naming a factory-backed
tool without binding the `sandbox` dep fails the build.

Built-in middleware kinds map 1:1 to the constructors in
`sdk/tool/middleware`. Custom kinds register on the `Builder` via
`RegisterFactory`; external sources (e.g. MCP) register via
`RegisterSourceFactory`.

A graph engine binds a `tool.Assembly` through its `tools` dep and the
executor becomes the dispatch target for `tool` nodes and for inference-driven
tool calls.

## Testing

Two complementary patterns:

**Tool-level unit test** — call `Execute` directly with a JSON argument
string. This is the right level for testing argument parsing, error wrapping,
and result shape.

**Middleware unit test** — build a `Dispatch` stub returning a canned
`message.Result`, wrap it in the middleware under test, and assert on the
produced result and the stub's call count. No registry needed.

**End-to-end** — parse a `tools.yaml` fixture, build the assembly, dispatch a
call, assert on the result — the same shape as the
[deploy guide's testing section](deploy.md#testing-your-deployment).

## Further reading

- Primitive contract: `sdk/tool/tool.go`, `sdk/tool/registry.go`,
  `sdk/tool/executor.go`, `sdk/tool/middleware.go`, `sdk/message/tool.go`,
  `sdk/message/schema.go`.
- Built-in middleware: `sdk/tool/middleware/doc.go` (per-middleware
  semantics).
- Built-in tools: `sdkx/tool/doc.go` (the curation rule),
  `sdkx/tool/{exec,askuser,delegation,mcp}/doc.go`.
- Assembly: `sdk/tool/config/doc.go`, `sdk/tool/config/document.go`, the
  `tool.Assembly` resource in [deploy.md](deploy.md#first-party-impls).
