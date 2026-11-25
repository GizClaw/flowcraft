---
layout: default
title: Tool System
---

# Tool System Guide

`core/tool` defines the LLM function-calling contract, a registry/catalog
directory, the executor dispatcher, and a middleware chain.

## Roles

| Type       | Responsibility                           |
| ---------- | ---------------------------------------- |
| `Registry` | mutable, thread-safe directory of tools  |
| `Catalog`  | read-side view (`Get`, `Definitions`)    |
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

## Asking before running

A tool that sometimes needs an answer before it may act implements one more method:

```go
type Asker interface {
    Ask(ctx context.Context, b Binding, arguments string) (*message.Content, error)
}
```

The tool node asks every pending call **before it launches the batch**. A non-nil
answer is a question, and the step suspends on it with nothing executed — so the
resume replays no side effect, and the turn ends `input-required` instead of
holding its slot (a blocking tool would hold the turn, and a synchronous
delegation's parent could never answer it).

Asking is side-effect free by contract: it reads the tool's own policy and returns
what the caller has to decide. A nil question means "run it". The dispatcher form
is the same idea one layer up:

```go
type AskDispatcher interface {
    Ask(ctx context.Context, b Binding, call message.ToolCall) (*message.Content, error)
}
```

It is optional on both sides — a tool or dispatcher that does not implement it runs
as it always did — and it deliberately bypasses the middleware chain, because a
middleware that counts, paces or audits executions must not see one that never
happened.

Two tools ship with this shape: `bash` (`tool/builtin/exec`), whose commands may
need an approval, and `ask_user` (`tool/builtin/ask`), which exists to ask. Both
are answered through `host.Agents.Resume`: a delegated child's question goes to
the parent that opened it, and a root session's goes to the outlet installed with
`host.WithQuestions`.

## Tool sources and the dynamic catalog

Tool sources feed the runtime's registry: the built-ins, the MCP servers above, and
anything the host passes to `runtime.WithToolSource`.

Dynamic injection is configured under `tools.dynamic`:

```yaml
tools:
  enable: [read, write, exec, tool_search] # empty = every registered tool
  dynamic:
    default: deferred # always | direct | deferred | hidden
    always: [tool_search] # forced visible every round
    hidden: [write] # never projected
    budget: 32 # visible definitions per round
    max_bytes: 16384 # their encoded size
    idle_rounds: 10 # pool eviction
    search_with_load: false # tool_search returns and loads in one step
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
    defer: false # eager: every server starts with the runtime
    servers:
      - name: filesystem
        transport: stdio # stdio | http
        command: npx
        args: ["-y", "@modelcontextprotocol/server-filesystem"]
        env: { TOKEN: "${MCP_TOKEN}" }
        prefix: fs # tool namespace; default "<name>__"
        description: "Workspace files" # what search can rank before connecting
        resources: true # bridge list_resources / read_resource tools
        required: true # the runtime fails if it cannot connect
        liveness: 30s # probe interval; "off" disables pings
      - name: remote
        transport: http
        url: https://mcp.example.com/mcp
        headers: { Authorization: "Bearer ${MCP_TOKEN}" }
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
A `http_timeout` bounds each request but does not enable transport-level
retries, because `tools/call` is not idempotent: a replayed timeout or 5xx would
run the tool twice.

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
configured takes the one-line form only; `params` needs the object form. `text` may
be omitted when only parameters are reworded — the tool keeps its own description:

```yaml
tools:
  descriptions:
    grep:
      params:
        max_matches: "Keep it small; 50 is usually enough."
```

Semantics, in full:

- **Replacement, never composition.** The configured text is the text: nothing of
  the generated description survives. Composition was rejected because the common
  motive for overriding is deleting a sentence that is wrong for this deployment,
  and a template cannot delete. What a deployment starts from is the assembled
  text itself, not a guess at the Go source: `host.Manager.ToolDefinitions` returns
  the definitions an agent's model reads, descriptions and parameter texts
  included.
- **Parameters are text too, and only text.** `params` rewrites
  `properties.<name>.description` of the tool's input schema. Types, `required`,
  bounds and `enum` stay exactly as the tool declared them, because those are what
  the arguments are validated against. Only top-level properties are reachable, and
  a parameter whose schema is not an object — a boolean or a bare `null`, both legal
  JSON Schema — has nowhere to put a description, so naming it is a build error.
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
  under another. A deployment that configures nothing keeps its digest: the field
  is omitted when empty.
- **A description is a configuration value, so `${...}` is a reference.** A layer
  expands references in every string it reads, so write `$${NAME}` — or `$$`
  generally — for a literal dollar that must reach the model.
- **A name that matches nothing fails the build.** An unknown tool name, or an
  unknown parameter name for a wired tool, is a configuration error reported with
  the names that exist. A tool that only a deferred MCP server publishes is not
  listable at build time, so it is reported as unknown too: a typo that waits is a
  typo nobody finds.

## Pinning the shell

The `exec` tool runs one command line through a shell. Which shell is a platform
fact, so by default the platform probes the host: PowerShell 7, then Windows
PowerShell, then `cmd` on Windows; `bash`, then `sh`, elsewhere. On Windows the
probe also refuses a Microsoft Store App Execution Alias (`…\WindowsApps\pwsh.exe`
is a reparse point a confined token cannot follow, so it looks like a shell and
starts nothing) and falls back to the well-known install locations when the
shell's directory is not on `PATH`.

A deployment that knows better says so under `tools.exec.shell`:

```yaml
tools:
  exec:
    shell:
      name: pwsh # tool name the model sees; default: base name of path
      path: 'C:\Program Files\PowerShell\7\pwsh.exe'
      args: [-NoProfile, -NonInteractive, -Command] # default for pwsh
```

- **`path` is required.** An absolute path, or a bare name resolved through
  `PATH` on the host that runs commands. It is not checked against this host's
  filesystem, because with `sandbox.backend: remote` the shell runs on the far
  side. A path under `WindowsApps` is refused: no token can start it.
- **`name` is the tool name, and the name approval classifies.** It must be one
  of the shells the platform knows — `bash`, `sh`, `zsh`, `dash`, `ash`, `ksh`,
  `fish`, `pwsh`, `powershell`, `cmd` — because that name selects the argv
  convention and the unwrapping the approval layer relies on. Empty takes the
  base name of `path` without `.exe`, so a pinned `pwsh.exe` is named `pwsh`. A
  stated name that disagrees with a known executable name is refused, because
  `-c` handed to `cmd.exe` runs nothing. The same name is what
  `tools.descriptions` keys off. A Windows install of a POSIX shell
  (`C:\Program Files\Git\bin\bash.exe`) is classified under its real file
  name, so pinning one keeps `bash.exe -c` on the approval path.
- **`args` is the prefix that precedes the command line.** Omitted, it follows
  from `name` (`-c`, `/c`, or `-NoProfile -NonInteractive -Command`). A
  deployment that writes its own is on its own: approval unwraps only the exact
  well-known form, so commands that would have been auto-approved ask the
  approver instead — a slower prompt, not a wider boundary.
- **It is part of the deployment's identity.** The shell enters the runtime
  digest, so changing it makes sessions started under the previous shell
  unresumable: the model was writing command lines for one grammar and would
  otherwise continue under another. A `tools.exec` block that pins nothing is
  omitted from the digest like an absent one, so `exec: {}` costs no checkpoints.
- **With `sandbox.backend: remote`, the probed shell is sent as a name.** The
  probe resolves an executable on the machine that builds the runtime, and the
  far side is the one that runs the command, so only the name crosses. A pinned
  `path` is sent as written: pin a name the far side resolves, or the path it
  actually has.
- **A host that gates commands by name must whitelist the resolved path.**
  `engine.AllowCommands` matches the command string exactly, and on Windows that
  string is now the absolute executable, so a gate that listed `pwsh` rejects it
  (the decorator's own doc says to normalise before calling `Exec`). Pinning
  `path: pwsh` sends the bare name again, with the alias risk back.
- **A pin no tool would run is a build error.** `tools.exec.shell` with `exec`
  absent from `tools.enable` is refused rather than silently ignored.

The session tools (`exec_session`, `write_stdin`) are unaffected: they take an
`argv` and start it directly, with no shell in between.

## Middleware chain

`tools.middleware` is the list above, applied in written order — first written is
outermost, which is what makes "recover outermost" expressible.

```yaml
tools:
  middleware:
    - name: telemetry
    - name: result_limit
      settings: { max_bytes: 20000 }
    - name: concurrency
      settings: { limit: 8 }
```

An empty list is not "no middleware": the runtime installs `recover` then
`telemetry`, because a panicking tool is foreign code (an MCP server, a plugin) and
losing the host to it is not a deployment choice. `recover` cannot be disabled
either — when the list omits it, it is prepended and the substitution is logged.
It converts a panicking tool (or inner middleware) into an `IsError` result, and
`Executor.Execute` still catches a panic escaping the whole chain.

| Name           | Settings                                   | Effect                                                                  |
| -------------- | ------------------------------------------ | ----------------------------------------------------------------------- |
| `telemetry`    | —                                          | an OpenTelemetry span per call, plus execution/duration/error metrics   |
| `recover`      | —                                          | a panic becomes an `IsError` result instead of a process exit           |
| `result_limit` | `max_bytes`, `marker`, `part_budget_bytes` | caps one result's text and the encoded size of its non-text parts       |
| `concurrency`  | `limit` (must be positive)                 | caps in-flight executions; excess callers wait, respecting cancellation |
| `redact`       | `patterns` (required)                      | masks secret-shaped substrings in results and arguments                 |

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
