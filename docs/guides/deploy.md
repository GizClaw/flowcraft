---
layout: default
title: Deployment Assembly
---

# Deployment Assembly Guide

`core/deploy` is the document and kind layer: it turns a deployment file into
`[]deploy.Spec` and decides which engine, protocol, hook and layer-source kinds a
host may name. It does not run anything. The process-level `Manager`, the session
table and the host bindings live in `core/agent-host`.

The split is deliberate: a document is data, a kind is a registration, and
neither needs a process to be validated. A host can lint a deployment in CI with
the same `Loader` it uses to start one.

## The unit is the deployment, not the process

One document declares one or more agents. Each agent is an engine plus the
substrate that engine runs on, and optionally an external surface:

```yaml
version: 1
agents:
  - name: forge
    engine:
      kind: graph
      settings: { ... } # decoded by the kind
    runtime:
      layers:
        - priority: 0
          source: { kind: file, settings: { path: base.yaml } }
        - priority: 10
          source: { kind: file, settings: { path: local.yaml } }
    catalog:
      scope: run
    protocol:
      kind: a2a
      settings: { ... }
    delegation:
      targets: [forge-review]
      modes: [sync]
    observability:
      trace: node
      bodies: false
    hooks:
      - kind: audit
        settings: { ... }
```

| Field                    | Rule                                                                                                              |
| ------------------------ | ----------------------------------------------------------------------------------------------------------------- |
| `version`                | Must be `1`. Anything else is rejected instead of being read as a coincidence.                                    |
| `agents[].name`          | Required, unique within the document, and what a caller addresses.                                                |
| `agents[].engine`        | Required. `kind` has no default: an agent that does not say what it is cannot be deployed meaningfully.           |
| `agents[].runtime`       | A stack of layers, at least one; each names where its bytes come from. Absent only for a remote engine.           |
| `agents[].catalog`       | `scope: run` (default) or `session`: how far a tool projection is scoped.                                         |
| `agents[].protocol`      | Absent means the agent is not exposed. `kind` is required when the block is present.                              |
| `agents[].delegation`    | Absent means the agent gets no delegation tools at all — not "unrestricted".                                      |
| `agents[].observability` | Absent fields fall back to the host's bindings, which is why the written form keeps "silent" distinct from "off". |
| `agents[].hooks`         | Chained in declaration order, before the host's own hooks.                                                        |

`version` stays `1` while the field-level tombstones name the replacement: a bump
would tell a reader "this build reads 2" and nothing about what to write.

Unknown fields, duplicate agent names and unregistered kinds are load errors. A
kind the document names but the host did not register fails with the registered
names in the error — never by being skipped.

## Kinds

A kind is a name, a decoder and a builder. Four kinds exist, one per thing a
document can name:

```go
type EngineKind struct {
    Name   string
    Local  bool                     // local: runs here, needs a runtime; remote: has a peer
    Decode func(ctx context.Context, settings json.RawMessage, sources SourceOpener) (any, error)
    // Fingerprint is the non-secret part of the settings. Nil means the whole
    // canonical settings object — wrong for anything carrying a credential.
    Fingerprint func(ctx context.Context, settings json.RawMessage, sources SourceOpener) string
    Build      func(ctx context.Context, deps EngineDeps) (agent.RunnerFactory, error)
}

type ProtocolKind struct {
    Name   string
    Decode func(json.RawMessage) (any, error)
    Build  func(ctx context.Context, deps ProtocolDeps) (Protocol, error)
}

type HookKind struct {
    Name   string
    Decode func(json.RawMessage) (any, error)
    Build  func(ctx context.Context, deps HookDeps) (agent.Hooks, error)
}
```

An engine is the only kind whose settings are validated at load time by reading a
document, and whose bytes are part of the deployment's identity, so its `Decode`
and `Fingerprint` receive the source table — and `EngineDeps.Sources` carries the
same opener to `Build`. (A hook can read a document of its own too, but through
`HookDeps.LoadOptions`, which is host wiring rather than a source reference.) The
table is the same one runtime layers resolve through
(see [Layer sources](#layer-sources)), which is why the two cannot disagree
about what a source kind means.

`Decode` runs twice by design: once at load, so a typo in a settings block fails
before any resource is built, and once inside `Build` to get the typed value. It
is the kind's only validation hook, and it is why an unregistered field inside
`settings` is a load error rather than a silently ignored line.

`Local` is what the loader checks instead of the kind's name: a local engine must
have a runtime, a remote one must not (`ValidateEngineSpec`). `ProtocolDeps`
carries no deployment internals — name, store, fingerprint, an artifact resolver
and the agent's session table — because a protocol drives sessions directly and
should not need the manager.

### Layer sources

A runtime layer names the kind its bytes come from instead of a path the document
interprets:

```yaml
runtime:
  layers:
    - priority: 0
      source: { kind: file, settings: { path: ./runtime.yaml } }
    - priority: 10
      source: { kind: assets, settings: { name: runtime/prod.yaml } }
```

```go
type SourceKind struct {
    Name string
    Open func(ctx context.Context, settings json.RawMessage) (Source, error)
}

type Source struct {
    Name string        // what errors and provenance report
    Body config.Source // config.File, config.Bytes, config.FS or config.Func
}
```

A source has one hook rather than two, because it runs while the document is
loaded: the layers are merged then, so `Open` is the only place a typo can be
caught, and there is nothing left to build afterwards.

`file` and `inline` are the two registrations `deploy/builtin` ships, and the
reason is the same for both: the document states the whole address, a path or the
text itself. Everything else needs a value only the host has — an `fs.FS` from
`//go:embed`, a vault client, a table handle — so `deploy/sources` offers a factory
instead of a registration:

```go
//go:embed assets
var assets embed.FS

// {"kind": "assets", "settings": {"name": "runtime/base.yaml"}}
kinds := builtin.Kinds().WithSources(sources.FS("assets", assets))
```

`deploy/sources` is the whole family, one factory per `config.Source`:

| Source                     | Settings        | Comes from                                                           |
| -------------------------- | --------------- | -------------------------------------------------------------------- |
| `file`                     | `{path}`        | `config.File` — a registration                                       |
| `inline`                   | `{name?, body}` | `config.Bytes` — a registration, for generated deployments and tests |
| a host name, e.g. `assets` | `{name}`        | `config.FS` — `sources.FS(name, fsys)`                               |
| a host name, e.g. `vault`  | `{name}`        | `config.Func` — `sources.Func(name, load)`                           |

An `inline` body is expanded once as part of the deploying document and again as a
layer, so a `${…}` inside one becomes layer source text: a body that carries a
reference has to escape its `$` one level deeper.

There is no HTTP, vault or database source in the platform, and that is the
point: each of those is a policy — TLS, retries, a size limit, credentials — and
`sources.Func` turns any of them into five lines the host owns rather than a
feature that decides them for every deployment.

A factory names its layers `<kind>:<reference>` (`assets:runtime/base.yaml`,
`vault:openai/luna`), so an error or a provenance entry says which store the
layer came from. The two registrations name themselves differently: `file` uses
the path it reads, because that is already a location, and `inline` uses its
`name` setting, defaulting to `inline` — which is why two anonymous inline layers
in one stack are a load error.

A source is opened more than once for one deployment (to validate, for the
engine's identity, to compile), so it must return the same bytes each time: a
loader that rotates a token per call makes the recorded identity advisory rather
than a description of what ran.

`Body` is the same `config.Source` a layer stack has always used, so nothing below
this seam changes: merge, placeholder expansion, provenance and `runtime.Digest`
read bytes. `Source.Name` is what errors and provenance report, which is why
it exists at all — a layer read from an embedded tree has no path to print.

An engine can name a document the same way. The graph engine's settings are a
source reference and nothing else:

```yaml
engine:
  kind: graph
  settings:
    source: { kind: assets, settings: { name: graphs/react.yaml } }
```

It goes through the same `Kinds.Sources` table, which is the point: a host
registers `assets` once and both its runtime layers and its graph documents come
from the embedded tree. The engine opens the source at load (to validate the
document), for the fingerprint (the bytes are part of the deployment's identity)
and once more when the graph is compiled.

The deployment document itself is the third user: `Loader.LoadSource` takes the
same reference, which is what makes `LoadFile` sugar over
`{kind: file, settings: {path: ...}}` rather than a second rule about where a
document may live. `Loader.Load` stays the entry for a body the host already
holds.

### What ships

```go
deploy/builtin — Kinds()
// Engines:      a2a (deploy/engines/a2a, core/agent-a2a client), graph (deploy/engines/graph, core/agent-graph)
// Protocols:    a2a (deploy/protocols/a2a, which holds the server and the kind)
// Sources: file and inline (deploy/sources); the FS and Func factories are the host's to register
// Hooks:        none — a host's policy is the host's
```

A hook is the one kind core ships none of, and the reason is what the four kinds
are. An engine, a protocol and a layer source are mechanisms: a way to run a
graph, to speak A2A, to read bytes. A hook is a policy attached to an agent's
life — memory, an audit trail, a seat binding — and building one means reading
the host's own store, workspace and session table. The platform ships the seam
(declaration order, `HookDeps`, load-time validation through `Decode`) and no
policy, so nothing decides how a deployment behaves on the host's behalf.
Everything hook-shaped that core already has is a binding instead:
`Bindings.OnEvent`, `Bindings.OnRun`, the delegation service, the sandbox
journal. `examples/forge` is what a real one looks like: `WithHooks(memory.Kind())`,
registered by the host that owns the memory.

Each registration ships beside the package that implements it, and `deploy`
itself imports none of them: a host that runs one engine should not link the
other, which a hardcoded table in `deploy` would make impossible. The opt-in
packages mirror the four tables of `deploy.Kinds` — `deploy/engines/<kind>`,
`deploy/protocols/<kind>`, `deploy/sources` for the sources and
`deploy/builtin` for the whole set — and the asymmetry between them is content,
not shape.
An engine kind is a registration only, because its implementation is a lower
layer (`core/agent-graph`, `core/agent-a2a`) that must not import `deploy`. The
A2A protocol kind has no such lower home: it is driven by a deployment, so the
server travels with its registration. The payoff is an import closure a host
chooses:

```go
import (
    "github.com/GizClaw/flowcraft/core/deploy"
    enginesgraph "github.com/GizClaw/flowcraft/core/deploy/engines/graph"
    "github.com/GizClaw/flowcraft/core/deploy/sources"
)

// A document is read through the source table, so a host that builds its own
// table names at least one source: the deployment document itself comes through
// it too.
kinds := deploy.Kinds{}.
    WithEngines(enginesgraph.Engine()).
    WithSources(sources.File())
```

`deploy/builtin` is the convenience trade for a host that wants every kind in one
line: it imports every registration, so the binary carries the graph runtime and
the A2A client and server. `deploy` alone is the document format and nothing
else.

Extend with copies, never by mutating the shared tables:

```go
kinds := builtin.Kinds().
    WithEngines(myEngine()).
    WithHooks(myHook()).
    WithProtocols(myProtocol())
```

A manager handed a zero `deploy.Kinds` registers nothing and deploys nothing — a
document then fails to load with the offered names in the error. Pass
`builtin.Kinds()` to start from the shipped set.

## Placeholders

Every string in the document may name a placeholder. Three forms:

| Form                | Resolves through                                 |
| ------------------- | ------------------------------------------------ |
| `${NAME}`           | the built-in `env` scheme (`SchemeEnv`)          |
| `${NAME:-fallback}` | `env`, with a default when unset                 |
| `${scheme:arg:arg}` | a resolver registered with `config.WithResolver` |

Arguments after the scheme are split on `:` and handed to the resolver
untouched — `${secret:openai:luna}` reaches the `secret` resolver as
`["openai", "luna"]`. Whether those name a vault path, an environment variable or
a row in a table is the host's business; `deploy` never interprets them.

Expansion happens once, on the parsed document, before decoding, so a secret can
appear in `peer`, `auth`, a card or `settings` with the same rule. `Loader`
applies the same mechanism to the outer document that a runtime layer uses
internally.

To print a startup checklist instead of a restart loop, ask first:

```go
req, err := loader.RequiredPlaceholdersFile(ctx, "deploy.yaml")
// req lists every unset reference in one pass; Load stops at the first.
```

### A document an engine reads of its own

An engine may name a document beside the deployment's settings, and the graph
engine does: its node config. That document is expanded by the same code and the
same resolvers, so `${secret:agent:prompt}` in a node config resolves like one in
`settings`.

The wiring takes two lines because the resolver is needed at two moments. The
loader validates the graph document while decoding the deployment's settings, and
`EngineKind.Decode` carries no `LoadOptions` — so the resolvers reach it at
construction, on the engine kind. A `Spec` composed in code never goes through
the loader at all, so the same slice is handed to the bindings for the build:

```go
resolvers := []config.Option{config.WithResolver("secret", vault.Resolve)}
kinds := builtin.Kinds().WithEngines(
    enginesgraph.Engine(nodes.WithConfigOptions(resolvers...)),
)
host.New(kinds, host.Bindings{LoadOptions: resolvers})
```

A host that only deploys documents can leave the bindings entry out; one that
only composes specs can leave the engine option out. A graph document is expanded
before it is decoded either way, so a node config never receives a literal
`${...}` — an unregistered scheme is a load error listing the schemes that are
registered.

A load that cannot expand the graph document names every reference it makes, not
just the one it stopped at, so the failure a host prints at startup is the whole
checklist. `nodes.RequiredPlaceholders` answers the same question on demand, for
a host that holds the document before it loads it:

```go
requirement, err := nodes.RequiredPlaceholders(ctx, path, config.File(path))
// requirement.Required mixes environment variables with "scheme:args" names;
// Requirement.Missing covers the environment variables only.
```

## From document to spec

```go
loader := deploy.Loader{
    Kinds:       kinds,
    LoadOptions: []config.Option{config.WithResolver("secret", vault.Resolve)},
}

// The document itself is read through the same source table as everything else.
specs, err := loader.LoadSource(ctx, &deploy.SourceDocument{
    Kind:     "assets",
    Settings: json.RawMessage(`{"name":"deploy.yaml"}`),
})

// The three entries, in order of how much the host has already done:
//   LoadSource  — the host names a source kind; the loader reads it.
//   LoadFile    — LoadSource with the file source: {kind: file, settings: {path}}.
//   Load        — the host already holds the bytes (HTTP, a database, a test).
```

A `deploy.Spec` is the validated form of one agent:

| Field                        | Meaning                                                                                                           |
| ---------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| `Name`                       | The agent's address.                                                                                              |
| `Engine`                     | Kind plus raw settings; the kind decodes them again when it builds.                                               |
| `Protocol`                   | Nil when the agent is not exposed.                                                                                |
| `Runtime`                    | A resolved `runtime.Config` — layers already merged and expanded. Absent for a remote engine.                     |
| `Catalog`                    | Validated projection scope.                                                                                       |
| `Delegation`                 | Nil when the document declared none.                                                                              |
| `Observability`              | "Silent" is an empty `Trace` and a nil `Bodies`, not zero values.                                                 |
| `Hooks`                      | In declaration order.                                                                                             |
| `IdleTimeout`, `MaxSessions` | Host decisions, not document fields: a session lives in the store, so eviction is a cache concern, not data loss. |

Errors are `errdefs.Validation` and name the location, e.g.
`agents[forge].engine: kind "grpah" is not registered; offered: a2a, graph`.

## Running the specs

`core/agent-host` turns specs into deployed agents:

```go
manager := host.New(kinds, host.Bindings{
    Store:           sqliteStore,         // nil keeps sessions in memory only
    RuntimeOptions:  []runtime.Option{runtime.WithDrivers(driver)},
    SandboxBackends: backends.All(),      // the sandbox implementations this process ships
    LoadOptions:    []config.Option{config.WithResolver("secret", vault.Resolve)},
    OnEvent:        func(agentName, sessionID string, event agent.Event) { ... },
    Trace:          telemetry.TraceRun,   // for agents whose document is silent
    Harness:        "forge@1.2.3",
})

names, err := manager.Deploy(ctx, specs...)

sess, _ := manager.Session("forge", sessionID)
for event, err := range sess.Run(ctx, message.NewTextMessage(message.RoleUser, "hi")) { ... }
```

| Call                         | Use                                                               |
| ---------------------------- | ----------------------------------------------------------------- |
| `Deploy`                     | Add or replace a batch of specs.                                  |
| `Add` / `Replace` / `Remove` | Single-agent management.                                          |
| `Handler(name)`              | The protocol's `http.Handler`, for `net/http`.                    |
| `Session(name, id)`          | The session table entry: `Run`, `State`, `Cancel`, `Result`.      |
| `Drain` / `Close`            | Stop accepting, then tear down. `Drain` finishes what is running. |

Callbacks run synchronously on the run's goroutine, so they must be fast and
non-blocking: a slow `OnEvent` stalls the run and its protocol stream.

## Where each piece lives

| Package                     | Role                                                                                            |
| --------------------------- | ----------------------------------------------------------------------------------------------- |
| `core/deploy`               | Documents, specs, the kind tables, loading, placeholders. `deploy/builtin` is the whole set.    |
| `core/deploy/engines/graph` | Registration of the `graph` engine.                                                             |
| `core/deploy/engines/a2a`   | Registration of the `a2a` engine (a peer driven as an engine).                                  |
| `core/deploy/protocols/a2a` | The `a2a` protocol kind: registration, server, executor and task store.                         |
| `core/agent-graph`          | The graph engine's implementation; see [graph.md](graph.md).                                    |
| `core/agent-a2a`            | The A2A client, which the `a2a` engine wraps.                                                   |
| `core/agent-host`           | Manager, bindings, session table, deployment lifecycle.                                         |
| `core/runtime`              | The substrate one agent runs on; see [runtime.md](runtime.md).                                  |
| `core/delegation`           | Target discovery and handoff behind the `delegation` block. See [delegation.md](delegation.md). |

## What was removed, and why

The previous assembly model — `resources: {kind, impl, deps}`, a dependency DAG
and a `Build` phase over it — is gone. It made every policy a resource and every
resource a factory, which meant a deployment could not be read without running
its dependency graph in your head. The current model has four kinds, each of
which is a thing a document can name, and one `Loader` that validates them
without building anything.
