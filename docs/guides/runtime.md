---
layout: default
title: Application Runtime
---
# Application Runtime Guide

`core/runtime` assembles one agent runtime from a configuration document and
owns everything it built.

The unit is deliberately not a session. A `Runtime` holds the substrate a turn
runs on — the model provider and its router, the tool registry with its
instances, the sandbox runner, the file boundary, the skill index, the journal —
and nothing that belongs to a conversation: no history, no turn counter, no
per-user state. Two runtimes can live in one process without sharing anything,
which is what makes "atomic" true rather than aspirational. History is an
argument to a turn, and the turn loop belongs to whatever layer adds prompting
(`core/agent`, `core/agent-host`).

## The document

```yaml
version: 1
name: forge

files:
  roots:
    - /workspace/forge            # absolute; the first root is the default workdir

sandbox:
  backend: seatbelt               # local | seatbelt | bwrap | windows | remote
  writes:
    - /workspace/forge/out
  net:
    mode: allow_list
    allow_hosts: [api.openai.com, "*.github.com"]
  timeout: 60s
  cpu_millicores: 2000            # needs timeout
  memory_mib: 2048
  output_mib: 8
  journal:                        # presence opts in; absent means no journal
    exclude: [.git, node_modules]
    retention: 4096

inference:
  providers:
    - name: openai
      driver: openai
      settings: { api_key: "${OPENAI_API_KEY}" }
  models:
    - provider: openai
      model: gpt-5
      weight: 100
      capabilities: { inputs: [text, image] }
      limits: { max_input_tokens: 200000, max_output_tokens: 32000 }
  route:
    selector: weighted
    attempts_per_target: 2
    max_targets: 3

tools:
  enable: [exec, read, write, edit, glob, grep, skill_search, skill_read]
  tty: false
  dynamic:
    default: direct
    hidden: [write]
  images: false
  mcp:
    servers: []
  middleware:
    - { name: redact, settings: { pattern: "sk-[A-Za-z0-9]+" } }

skills:
  roots: [.agents/skills]
```

### `sandbox`

The process boundary. Nothing here is guessed:

| Field | Meaning |
| --- | --- |
| `backend` | Required. `local`, `seatbelt`, `bwrap`, `windows`, or `remote`. There is no default: a boundary picked by guessing is a boundary nobody chose. |
| `workdir` | Where commands run. Empty means the first files root. |
| `writes` | Absolute paths a command may write. Empty means read-only. |
| `net.mode` | `default`, `none` (aliases `deny`, `deny_all`), `allow_list` (alias `allowlist`), `proxy` (alias `proxies`). |
| `net.allow_hosts` | Only legal with `allow_list`; a host pattern or `*`-glob. |
| `net.proxy` | Only legal with `proxy`. |
| `timeout` | Default per-call deadline. A tool may ask for less; this is the ceiling. |
| `cpu_millicores` | Millicore budget. Rejected without `timeout`, because the budget is derived from it. |
| `memory_mib` | Address-space cap; zero is unbounded. |
| `output_mib` | Captured output per call; zero uses the backend default. |
| `journal` | A settings block. Presence opts in; see [sandbox.md](sandbox.md). |
| `bwrap`, `seatbelt`, `windows` | The backend's own construction options. Setting one on a different backend is a validation error, not a comment. |
| `remote` | Client configuration for `backend: remote`; must be absent otherwise. |

### `files`, `skills`, `tools`

| Section | Rule |
| --- | --- |
| `files.roots` | At least one, all absolute; this is also the write policy the file tools enforce. |
| `skills.roots` | Directories scanned for `SKILL.md`. One index is built and shared by the tools and the host's own prompts. |
| `tools.enable` | Empty means every registered tool. Names not registered are a build error. |
| `tools.dynamic` | The projection: `default` (`always` / `direct` / `deferred` / `hidden`), plus `always`, `hidden`, `budget`, `max_bytes`, `idle_rounds`, `search_with_load`. |
| `tools.middleware` | Named middleware with kind-owned settings, applied in order. |

### `inference`

Providers are `{name, driver, settings}`; the driver name must be registered on
the `Runtime` (see `WithDrivers`). Models are `{provider, model, weight,
capabilities, limits}`. `route` decides how a request picks among them:
`selector` plus `selector_settings`, `attempts_per_target`, `max_targets`,
`base_delay`, `max_delay`. See [inference.md](inference.md).

## Layers

A deployment usually stacks three files: a base, a profile, a local override.

```go
cfg, provenance, err := runtime.LoadLayers(ctx, []runtime.Layer{
    {Priority: 0,  Name: "base.yaml",    Path: "base.yaml"},
    {Priority: 10, Name: "profile.yaml", Path: "profile.yaml"},
    {Priority: 20, Name: "local.yaml",   Bytes: embedded},
}, runtime.WithResolver("secret", vault.Resolve))
```

- Layers merge in **ascending priority**; a higher priority wins.
- Duplicate priorities are rejected. Order in the slice is not order of
  application — a profile picked on a command line belongs above a file from a
  directory scan regardless of what the scan produced.
- Merge rules are per node kind and nothing is inferred by reflection: mappings
  merge key by key, **sequences are replaced whole**, scalars are replaced. A
  permission list must not grow silently because someone added one line.
- Placeholders are resolved **once, after the merge**, so a value an overriding
  layer replaced is never looked up. Resolvers may read a secret store, log an
  access or fail; doing that for dead values is waste. Forms: `${NAME}`,
  `${NAME:-fallback}` (both `env`), and `${scheme:arg:arg}` for anything
  registered with `runtime.WithResolver`.
- `provenance` maps a dotted path to the layer that last set it — the answer to
  "which file put this value here", which a merged policy otherwise lacks.

## Building

```go
// Resolvers are a load option: they belong to reading the document.
cfg, provenance, err := runtime.LoadLayers(ctx, layers,
    runtime.WithResolver("secret", vault.Resolve),
)

rt, err := runtime.New(ctx, cfg,
    // Drivers are keyed by the name written in inference.providers[].driver.
    runtime.WithDrivers(runtime.Driver{Name: "openai", Build: openai.New}),
    // Selectors are keyed by the name written in inference.route.selector.
    runtime.WithSelectors(runtime.Selector{
        Name:     "weighted",
        Generate: func(json.RawMessage) (route.Selector[inference.GenerateRequest], error) {
            return weighted{}, nil
        },
    }),
    runtime.WithToolSource(myExtraTools),
    // Middleware that needs a Go value cannot come from YAML, so the host
    // registers it under the name the document lists.
    runtime.WithMiddleware(runtime.MiddlewareOption{Name: "approve", Build: approve}),
)
defer rt.Close()
```

There are no built-in drivers or selectors: a name the document writes must have
been registered, and an unregistered one fails with the offered names —
`inference.route.selector: "cheapest" is not available; offered: weighted`. An
empty `selector` keeps declaration order. Middleware is the exception: the built-in
names `telemetry`, `recover`, `result_limit`, `concurrency` and `redact` are
always available, in the order the document lists them, first written outermost.

`New` validates before it builds, so a document that cannot be honoured fails
without side effects:

- `files.roots` present, absolute, and accepted by `permission.Writes`;
- `sandbox.backend` known, `net.mode` known, `net.allow_hosts` only with
  `allow_list`, `timeout` and `remote.timeout`/`poll_hold` parseable and positive;
- `cpu_millicores` implies a `timeout`, and the backend can actually enforce a
  cpu or memory cap — see below;
- a backend's own block (`bwrap`, `seatbelt`, `windows`) may only appear with
  that backend, and `remote` only with `backend: remote`;
- the journal block's cheap invariants (op names, non-negative retention and
  watch budget). Root resolution stays a build concern, so a platform without a
  watch source fails here rather than silently reporting nothing.

## The sandbox policy reaches the commands

This is the part that used to be missing: a document could declare a budget that
nothing enforced. The runtime now projects all four knobs onto every call it
issues through `engine.WithDefaults`:

| Document | Becomes |
| --- | --- |
| `timeout` | `ExecOptions.Timeout` — a ceiling; a tool asking for less keeps its own. |
| `cpu_millicores` | `Resources.CPU` (derived from the timeout). |
| `memory_mib` | `Resources.MemoryBytes`. |
| `writes` | `Permission.Writes` — the files a command may touch. |

Three rules make that honest:

1. **Stricter wins, per field.** When a tool also sets resources, the two are
   merged field by field: zero defers, and of two non-zero values the smaller
   one wins. A tool cannot widen the deployment's budget and the deployment
   cannot silently widen a tool's.
2. **An unenforceable cap is a build error.** `writes` is only projected onto a
   backend that has write confinement; asking for a cap a backend cannot apply
   names the field instead of pretending.
3. **`writes` on `local`/`remote` warns.** `local` enforces no boundary and
   `remote` enforces it on the server, so the value would be decoration; the
   runtime logs a warning rather than implying enforcement.

## The remote backend

`sandbox.backend: remote` hands execution to a server that speaks the wire
protocol in `core/sandbox-remote/wire`:

```yaml
sandbox:
  backend: remote
  remote:
    transport: http              # http (default) | unix
    endpoint: https://sandbox.internal:8443
    token: "${SANDBOX_TOKEN}"
    timeout: 30s
    poll_hold: 15s
    insecure_skip_verify: false
```

| Transport | Fields |
| --- | --- |
| `http` | `endpoint` required and `http(s)`; `socket` rejected. |
| `unix` | `socket` required; `endpoint` and `insecure_skip_verify` rejected. A token is optional — the boundary is the directory's permissions. |

Every other backend's fields behave the same way: the field the transport does
not use is a validation error, because a socket path beside an endpoint is
configuration that silently does nothing.

## Accessors

| Call | Returns |
| --- | --- |
| `Config()` | The resolved configuration, layers already merged. |
| `Provider()`, `Router()`, `DefaultModel()` | The inference side. |
| `Tools()`, `Assembly()`, `NewCatalogView()` | The tool registry and the per-run projection. |
| `Runner()`, `Journal()` | The sandbox boundary and its file journal (nil when absent). |
| `Files()` | The `fsx.Tree` the file tools write through. |
| `Skills()` | The one `skill.Index` the tools and the host share. |
| `Context(ctx)` | A context carrying the runtime's identity for tracing. |
| `Close()` | Tears down, in reverse construction order. |

## Ownership

One rule: **a `Runtime` closes what it created.** A value handed in through an
option is somebody else's to close, and `Close` leaves it alone.

`Close` runs in reverse construction order — tools, then journal, then runner,
then tree — because a session that outlives its runner writes into a corpse, and
a journal that outlives its runner drops the last events. Errors are joined: a
failing journal must not stop the runner from being closed.

## What is not here

| Not in `runtime` | Where it lives |
| --- | --- |
| Sessions, history, turn loop | `core/agent` and `core/agent-host` |
| Lifecycle hooks | `agent.Hooks`, attached by a `deploy.HookKind` — see [hooks.md](hooks.md) |
| Events a UI subscribes to | `agent.Event`, streamed by a session |
| Reload / hot swap | A host builds a new `Runtime` and drains the old one |
| Deployment documents | `core/deploy` — see [deploy.md](deploy.md) |
