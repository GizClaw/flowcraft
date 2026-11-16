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

## Tool sources and the dynamic catalog

Tool sources feed the runtime's registry: the built-ins, the MCP servers above, and
anything the host passes to `runtime.WithToolSource`.

Dynamic injection is configured under `tools.dynamic`:

```yaml
tools:
  enable: [read, write, exec, tool_search]   # empty = every registered tool
  dynamic:
    default: deferred          # always | direct | deferred | hidden
    always: [tool_search]      # forced visible every round
    hidden: [write]            # never projected
    budget: 32                 # visible definitions per round
    max_bytes: 16384           # their encoded size
    idle_rounds: 10            # pool eviction
    search_with_load: false    # tool_search returns and loads in one step
```

`tool_search` is the discovery tool: a query plus an optional limit.
Matching tools are loaded and added to the session's discovery pool
automatically (no `select` step), and their real schemas become visible
from the next round, subject to the per-round `budget`. The result
reports only what will actually be sent: a hit that loses the per-round
budget is not listed in `exposed`, it comes back under `failed` with
reason `visible_budget`. Hits are ranked, so a batch's first (best) hit
wins when the budget cuts the set, and an oversized definition is
skipped instead of truncating everything behind it.

Hits are ranked by the shared BM25 kernel (`core/internal/search`): a name
match outweighs a description match, an exact term outweighs a term it
merely prefixes, one query term contributes its best match and never the
sum of everything it prefixes, and equal scores keep name order. ASCII
terms shorter than two runes are ignored on both sides, so a filler word
like `a` neither matches nor widens the hit set, while CJK splits into
single characters plus adjacent bigrams, so `会话` matches `清理会话`
without a segmentation dictionary. `limit` is capped at
`dynamic.max_definitions`: hits beyond the pool could never be exposed, and
loading them would only evict tools the session is using.

Pool entries stay visible while they are used: every executed call
refreshes the entry, idle entries are evicted after
`dynamic.idle_rounds`, and the pool is bounded by `dynamic.budget` and
`dynamic.max_bytes`. A pool kept larger than the per-round budget acts as a
loaded cache that costs no tokens: those entries return to the visible set when
used or re-searched.
`tool_search` must stay `always`; a different exposure is rejected at
assembly build time.

MCP servers are declared in the runtime document, under `tools.mcp`. Two modes,
one visible difference: eager starts every server at assembly, deferred starts one
when a tool from it is loaded.

```yaml
tools:
  mcp:
    defer: false                   # eager: every server starts with the runtime
    servers:
      - name: filesystem
        transport: stdio           # stdio | http
        command: npx
        args: ["-y", "@modelcontextprotocol/server-filesystem"]
        env: {TOKEN: "${MCP_TOKEN}"}
        prefix: fs                 # tool namespace; default "<name>__"
        description: "Workspace files"   # what search can rank before connecting
        resources: true            # bridge list_resources / read_resource tools
        required: true             # the runtime fails if it cannot connect
        liveness: 30s              # probe interval; "off" disables pings
      - name: remote
        transport: http
        url: https://mcp.example.com/mcp
        headers: {Authorization: "Bearer ${MCP_TOKEN}"}
        http_timeout: 30s
```

`required: true` means the runtime cannot be built without that server; without
it an unreachable server is retried in the background with backoff, and its tools
appear in the registry the moment it connects. A server that dies later is
reconnected the same way: its tools stay registered and calls fail with a
per-server `NotAvailable` until the connection is restored. A configuration error
— a rejected spec, an invalid transport — still fails the build.

The `prefix` is what keeps two servers from claiming one name, and it is the
namespace part of `<prefix>__<tool>`; a model-visible tool name has to match
`^[a-zA-Z0-9_-]+$`, which is why the separator is `__` and not `/`.

Before a server connects, only its `description` exists, which is all the search
kernel has to rank on. `liveness` overrides the source-wide probe interval for one
server. Servers on protocol `2026-07-28` or later are never pinged — those
revisions removed the method — they reconnect when their connection closes.

For a runnable anonymous search and fetch example over the HTTP bridge, see
[`examples/parallel`](https://github.com/GizClaw/flowcraft/tree/main/examples/parallel):
it connects a server, discovers its tools and executes them through the ordinary
registry and executor, with no model provider.

## Configuring a tool's description

A tool's description is prompt surface, not contract: it is how the model decides
which tool to call and what to put in its arguments. The same tool belongs to
deployments whose instructions differ, so the wording is configuration —
`tools.descriptions` replaces it per deployment.

```yaml
tools:
  descriptions:
    # whole description, replaced
    read_file: |-
      Read a text file and return one window of it. Paths are absolute.
    bash:
      text: |-
        Run one command line. There is no terminal: give every search a path.
      params:
        workdir: "Absolute directory to run in; stays under the workspace."
```

The one-line form is shorthand for `{text: ...}`. A tool whose parameters are not
configured takes the one-line form only; `params` needs the object form.

Semantics, in full:

- **Replacement, never composition.** The configured text is the text: nothing of
  the generated description survives. Composition was rejected because the common
  motive for overriding is deleting a sentence that is wrong for this deployment,
  and a template cannot delete. What a deployment starts from is the generated
  text itself — `forge describe tools <workspace>` prints the descriptions and
  parameter texts the runtime currently assembles, in this configuration's shape,
  ready to edit and paste back.
- **Parameters are text too, and only text.** `params` rewrites
  `properties.<name>.description` of the tool's input schema. Types, `required`,
  bounds and `enum` stay exactly as the tool declared them, because those are what
  the arguments are validated against. Only top-level properties are reachable.
- **Model-facing only.** The registry applies overrides when it builds
  `Definitions()`, so what the loop sends and what `tool_search` ranks on are the
  configured words. `Catalog.Get` still returns the tool, and its own
  `Definition()` still says what its author wrote.
- **Everything else is unchanged.** Execution, approval, middleware and the
  parameters a tool accepts are untouched by a description override.
- **It is part of the deployment's identity.** Descriptions enter the runtime
  digest and the request hash, so editing one makes sessions started under the
  previous text unresumable and invalidates its prompt cache. That is deliberate:
  the model was choosing tools by one description and would otherwise continue
  under another.
- **A name that matches nothing fails the build.** An unknown tool name, or an
  unknown parameter name for a wired tool, is a configuration error reported with
  the names that exist. A tool that only a deferred MCP server publishes is not
  listable at build time, so it is reported as unknown too: a typo that waits is a
  typo nobody finds.

## Middleware chain

`tools.middleware` is the list above, applied in written order — first written is
outermost, which is what makes "recover outermost" expressible.

```yaml
tools:
  middleware:
    - name: telemetry
    - name: result_limit
      settings: {max_bytes: 20000}
    - name: concurrency
      settings: {limit: 8}
```

An empty list is not "no middleware": the runtime installs `recover` then
`telemetry`, because a panicking tool is foreign code (an MCP server, a plugin) and
losing the host to it is not a deployment choice. `recover` cannot be disabled
either — when the list omits it, it is prepended and the substitution is logged.
It converts a panicking tool (or inner middleware) into an `IsError` result, and
`Executor.Execute` still catches a panic escaping the whole chain.

| Name | Settings | Effect |
| --- | --- | --- |
| `telemetry` | — | an OpenTelemetry span per call, plus execution/duration/error metrics |
| `recover` | — | a panic becomes an `IsError` result instead of a process exit |
| `result_limit` | `max_bytes`, `marker`, `part_budget_bytes` | caps one result's text and the encoded size of its non-text parts |
| `concurrency` | `limit` (must be positive) | caps in-flight executions; excess callers wait, respecting cancellation |
| `redact` | `patterns` (required) | masks secret-shaped substrings in results and arguments |

Non-text parts are bounded even without a `result_limit`: a tool result rides every
later turn's context, and an inline image has no natural size.
`DefaultResultPartBudget` is 1 MiB; `part_budget_bytes` moves it and `0` lifts it.
Whatever exceeds a budget is dropped and `DefaultResultMarker`
(`…[result truncated]`) is appended, so the model learns the result was shortened —
the marker's runes are reserved rather than added on top.

Middleware that needs a Go value is registered by the host and named by the
document: `middleware.Approval`, `middleware.Audit`, `middleware.RateLimit`,
`middleware.RecordCalls`, `middleware.Redact` and their kin, through
`runtime.WithMiddleware(runtime.MiddlewareOption{Name, Build})`. An unregistered
name is a build error with the offered list.

A model that calls a deferred tool before `tool_search` exposed it is rejected at
response validation with a distinguishable `undefined_tool` error rather than the
generic `provider_response_unusable`; the model node's recovery config turns that
into feedback that sends it back to `tool_search` on the next round (see
[graph.md](graph.md)).

See [runtime.md](runtime.md) for the per-session dynamic catalog.

## Catalog index reuse

`tool_search` ranks with BM25 over the searchable definitions, and building that
index means tokenizing every name and description. The assembly therefore keeps
one built index and shares it across the views it hands out, rebuilding only when
the catalog's definitions change. A catalog tells it so through an **optional**
capability, not through the `tool.Catalog` contract:

```go
// DefinitionsEpoch reports a counter that moves whenever the definition set may
// have changed (a tool registered, removed, or a deferred tool finishing its
// load). It is a hint for readers that cache something derived from
// Definitions; it is not part of Catalog.
DefinitionsEpoch() uint64
```

`tool.Registry` implements it. A catalog that does not — a fixture, a wrapper, a
third-party source — is still searchable; its index is simply rebuilt on every
`tool_search` call, which is what every catalog did before the cache existed. The
cache is per assembly, so a deployment keeps one index no matter how many
conversations it serves, and it holds only definitions: a tool list change is
visible on the next search rather than at the next restart.
