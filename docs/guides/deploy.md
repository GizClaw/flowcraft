---
layout: default
title: Deployment Assembly
---
# Deployment Assembly Guide

`core/deploy` is the document and kind layer: it turns a deployment file into
`[]deploy.Spec` and decides which engine, protocol and hook kinds a host may
name. It does not run anything. The process-level `Manager`, the session table
and the host bindings live in `core/agent-host`.

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
      settings: { ... }          # decoded by the kind
    runtime:
      layers:
        - { priority: 0, path: base.yaml }
        - { priority: 10, path: local.yaml }
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

| Field | Rule |
| --- | --- |
| `version` | Must be `1`. Anything else is rejected instead of being read as a coincidence. |
| `agents[].name` | Required, unique within the document, and what a caller addresses. |
| `agents[].engine` | Required. `kind` has no default: an agent that does not say what it is cannot be deployed meaningfully. |
| `agents[].runtime` | A stack of layers, at least one. Absent is only legal for an engine kind that reports `Local: false`. |
| `agents[].catalog` | `scope: run` (default) or `session`: how far a tool projection is scoped. |
| `agents[].protocol` | Absent means the agent is not exposed. `kind` is required when the block is present. |
| `agents[].delegation` | Absent means the agent gets no delegation tools at all — not "unrestricted". |
| `agents[].observability` | Absent fields fall back to the host's bindings, which is why the written form keeps "silent" distinct from "off". |
| `agents[].hooks` | Chained in declaration order, before the host's own hooks. |

Unknown fields, duplicate agent names and unregistered kinds are load errors. A
kind the document names but the host did not register fails with the registered
names in the error — never by being skipped.

## Kinds

A kind is a name, a decoder and a builder. Three kinds exist, one per thing a
document can name:

```go
type EngineKind struct {
    Name   string
    Local  bool                     // local: runs here, needs a runtime; remote: has a peer
    Decode func(json.RawMessage) (any, error)   // validates at load
    // Fingerprint is the non-secret part of the settings. Nil means the whole
    // canonical settings object — wrong for anything carrying a credential.
    Fingerprint func(json.RawMessage) string
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

`Decode` runs twice by design: once at load, so a typo in a settings block fails
before any resource is built, and once inside `Build` to get the typed value. It
is the kind's only validation hook, and it is why an unregistered field inside
`settings` is a load error rather than a silently ignored line.

`Local` is what the loader checks instead of the kind's name: a local engine must
have a runtime, a remote one must not (`ValidateEngineSpec`). `ProtocolDeps`
carries no deployment internals — name, store, fingerprint, an artifact resolver
and the agent's session table — because a protocol drives sessions directly and
should not need the manager.

### What ships

```go
deploy.BuiltinKinds()
// Engines:   a2a  (core/agent-a2a, remote client), graph (core/agent-graph)
// Protocols: a2a  (core/deploy/a2aprotocol, server)
// Hooks:     none — a host's policy is the host's
```

Registration lives here, not in the subpackages, and that is forced by the
import graph: `deploy/a2aprotocol` imports `deploy` for `ProtocolDeps`, so a
registration written there could never be part of `BuiltinKinds`. The rule is
one-directional — implementation in the subpackage, registration in `deploy`.

Extend with copies, never by mutating the shared tables:

```go
kinds := deploy.BuiltinKinds().
    WithEngines(myEngine()).
    WithHooks(myHook()).
    WithProtocols(myProtocol())
```

## Placeholders

Every string in the document may name a placeholder. Three forms:

| Form | Resolves through |
| --- | --- |
| `${NAME}` | the built-in `env` scheme (`SchemeEnv`) |
| `${NAME:-fallback}` | `env`, with a default when unset |
| `${scheme:arg:arg}` | a resolver registered with `runtime.WithResolver` |

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
req, err := loader.RequiredPlaceholdersFile("deploy.yaml")
// req lists every unset reference in one pass; Load stops at the first.
```

## From document to spec

```go
loader := deploy.Loader{
    Kinds:       kinds,
    LoadOptions: []runtime.LoadOption{runtime.WithResolver("secret", vault.Resolve)},
}

specs, err := loader.LoadFile(ctx, "deploy.yaml")
```

A `deploy.Spec` is the validated form of one agent:

| Field | Meaning |
| --- | --- |
| `Name` | The agent's address. |
| `Engine` | Kind plus raw settings; the kind decodes them again when it builds. |
| `Protocol` | Nil when the agent is not exposed. |
| `Runtime` | A resolved `runtime.Config` — layers already merged and expanded. Absent for a remote engine. |
| `Catalog` | Validated projection scope. |
| `Delegation` | Nil when the document declared none. |
| `Observability` | "Silent" is an empty `Trace` and a nil `Bodies`, not zero values. |
| `Hooks` | In declaration order. |
| `IdleTimeout`, `MaxSessions` | Host decisions, not document fields: a session lives in the store, so eviction is a cache concern, not data loss. |

Errors are `errdefs.Validation` and name the path, e.g.
`agents[forge].engine: kind "grpah" is not registered; offered: a2a, graph`.

## Running the specs

`core/agent-host` turns specs into deployed agents:

```go
manager := host.New(kinds, host.Bindings{
    Store:          sqliteStore,          // nil keeps sessions in memory only
    RuntimeOptions: []runtime.Option{runtime.WithDrivers(driver)},
    LoadOptions:    []runtime.LoadOption{runtime.WithResolver("secret", vault.Resolve)},
    OnEvent:        func(agentName, sessionID string, event agent.Event) { ... },
    Trace:          telemetry.TraceRun,   // for agents whose document is silent
    Harness:        "forge@1.2.3",
})

names, err := manager.Deploy(ctx, specs...)

sess, _ := manager.Session("forge", sessionID)
for event, err := range sess.Run(ctx, message.NewTextMessage(message.RoleUser, "hi")) { ... }
```

| Call | Use |
| --- | --- |
| `Deploy` | Add or replace a batch of specs. |
| `Add` / `Replace` / `Remove` | Single-agent management. |
| `Handler(name)` | The protocol's `http.Handler`, for `net/http`. |
| `Session(name, id)` | The session table entry: `Run`, `State`, `Cancel`, `Result`. |
| `Drain` / `Close` | Stop accepting, then tear down. `Drain` finishes what is running. |

Callbacks run synchronously on the run's goroutine, so they must be fast and
non-blocking: a slow `OnEvent` stalls the run and its protocol stream.

## Where each piece lives

| Package | Role |
| --- | --- |
| `core/deploy` | Documents, specs, kinds, loading, placeholders. |
| `core/deploy/a2aprotocol` | The A2A server (protocol implementation). |
| `core/agent-a2a` | The A2A client, as a remote engine kind. |
| `core/agent-host` | Manager, bindings, session table, deployment lifecycle. |
| `core/runtime` | The substrate one agent runs on; see [runtime.md](runtime.md). |
| `core/agent-graph` | The `graph` engine kind. See [graph.md](graph.md). |
| `core/delegation` | Target discovery and handoff behind the `delegation` block. See [delegation.md](delegation.md). |

## What was removed, and why

The previous assembly model — `resources: {kind, impl, deps}`, a dependency DAG
and a `Build` phase over it — is gone. It made every policy a resource and every
resource a factory, which meant a deployment could not be read without running
its dependency graph in your head. The current model has three kinds, each of
which is a thing a document can name, and one `Loader` that validates them
without building anything.
