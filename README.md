<div align="center">

# FlowCraft

**A Go platform for building AI agents: model calls, tools, sandboxed execution, and declarative graphs — assembled from YAML and owned by your process.**

[![CI](https://github.com/GizClaw/flowcraft/actions/workflows/ci.yml/badge.svg)](https://github.com/GizClaw/flowcraft/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/GizClaw/flowcraft/core.svg)](https://pkg.go.dev/github.com/GizClaw/flowcraft/core)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.26%2B-00ADD8.svg)](https://go.dev/dl/)

</div>

---

FlowCraft is a Go workspace for building agent deployments without tying
application code to one model provider or one execution model. A deployment is a
pair of documents: what this process carries, and which agents it serves. The
platform resolves them into something you own — one `Resource` per process, one
`Agents` table, one `Session` per conversation — and leaves prompting, persistence
and your product's own policies in your hands.

Authority is not a document: a session is opened with the `policy.Grant` its caller
states, so a gateway that knows who is asking decides what that conversation may
do. Graphs are one execution engine, not a required architecture: use the packages
directly, or start from the forge demo in `examples/forge`.

## Modules

- **`core`** — the platform module: the contracts (messages, errors, models,
  outcomes, files, skills, config, identity, permission), the inference and tool
  contracts, the sandbox contract, the `host` that runs one process and its
  sessions, the `deploy` documents and the telemetry.
- **`inference`** — the protocol families and provider adapters, one package per
  vendor: `openai`, `anthropic`, `bytedance`, `dashscope`, `minimax`, plus the
  request pipeline they share.
- **`sandbox`** — the backends: `local` (no OS boundary) and `remote` (the same
  `Runner` across a host boundary), with `netpolicy` and `sessionkit` beside them.
- **`agent`** — the engines: `graph` (nodes, script kinds, Starlark), `a2a` (client
  and wire translation) and `checkpoint` (the SQLite `agent.Store`).
- **`tool`** — the tool sources the platform ships: `builtin/{files,exec,skills,
delegation}`, `mcp`, and the `dispatch`/`middleware` engine.
- **`examples/forge`** — a runnable local demo: a werewolf table played by a cast
  of agents, with a TUI, scripted tests and self-play.
- **`examples/parallel`** — a standalone consumer: the HTTP MCP source bridged to
  Parallel's search server, deliberately pinned to an older `core` release.
- **`scripts/releasegate`** — release automation: changeset validation, release
  planning, changelog aggregation.

Modules are independently versioned; an application adopts only what it needs.

## The unit is the deployment

Two documents describe one deployment, and both are validated without being built:

| File               | Says                                                                                                                                                      |
| ------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| a process document | what this process carries: the sandbox backends, the model targets, the tool sources, the skill roots, the retry policy and how much of a turn it records |
| an agent document  | which agents exist, each with its program, the session table it runs under and its lifecycle hooks                                                        |

```yaml
# the agent document; the process document is the other half
version: 1
agents:
  - name: assistant
    program:
      impl: graph # registered by the application, not a built-in name
      settings:
        model: { provider: openai, name: gpt-4o-mini }
        path: ./graphs/assistant.yaml
    limits: { idle: 30m, max_sessions: 8 }
    hooks: [{ impl: memory, settings: { dir: ./.memory } }]
```

Everything a document can name is a registered implementation — a provider, a
backend, a tool source, a middleware link, a hook or a program — and one nobody
registered is a load error listing what was registered, never a silently skipped
block. Neither document states authority: what a session may do is the grant its
caller passes to `host.Open`.

## Quickstart

### Run the forge demo

```bash
cd examples/forge
cp .env.example .env          # then set OPENAI_API_KEY=...
go run . help
go run . tui new              # pick "werewolf" and start the table
go run . test -test werewolf/opening
go run . test-auto --raid werewolf --rounds 8
```

The demo is a host, not a framework: it opens a workspace, deploys agents, drives
a session and reads the events back — the same things any application writes.
Command reference and scenario layout live in
[`examples/forge/README.md`](examples/forge/README.md) (中文版:
[`README_zh.md`](examples/forge/README_zh.md)).

### Embed FlowCraft in a Go service

```go
impls := deploy.NewImpls().
    Provider(deploy.Impl("openai", buildOpenAI)).
    Backend(deploy.BackendImpl("local", newLocal)).
    ToolSource(deploy.Impl("files", builtinFiles)).
    Hook(deploy.Impl("memory", newMemory)).
    Program(deploy.Impl("graph", graphProgram))

loader := deploy.NewLoader(impls)
plan, err := loader.Process(ctx, processLayer)          // the process document
res, err := host.Open(ctx, plan, host.Options{
    Capabilities: capabilities,                          // declared by the application
    Dispatcher:   dispatcher,
    View:         view,
})
defer res.Close()

specs, err := loader.Deployments(ctx, agentLayer)       // the agent document
agents := host.NewAgents(res, host.WithStore(store), host.WithFingerprint(deploy.Fingerprint(specs)))
if err := agents.Add(specs...); err != nil { /* … */ }
defer agents.Close(ctx)

if _, err := agents.Open(ctx, "conversation-1", "assistant", authority); err != nil { /* … */ }
for event, err := range agents.Run(ctx, "conversation-1", message.NewTextMessage(message.RoleUser, "hello")) {
    if err != nil { break }
    render(event)                                        // text, reasoning, tool calls, usage
}
```

An implementation is a settings-plus-constructor pair the application registers,
and the adapter that maps a written `settings` block onto a provider's typed
settings belongs to your application — `examples/forge/internal/app` is a working
reference. See [`docs/guides/deploy.md`](docs/guides/deploy.md) and
[`docs/guides/runtime.md`](docs/guides/runtime.md).

## Architecture

`core` is layered: a package imports only from the layers below it, never from
the ones above.

```text
    ┌─────────────────────────────────────────────────────────────────┐
    │                         your application                        │
    │       providers · implementations · resolvers · approvals       │
    └─────────────────────────────────────────────────────────────────┘
                                     ▼
    ┌─────────────────────────────────────────────────────────────────┐
    │  deployment     host (process, sessions, turns)                 │
    │                 deploy (documents, implementation table)        │
    ├─────────────────────────────────────────────────────────────────┤
    │  authority      policy (grant, widening, denial)                │
    ├─────────────────────────────────────────────────────────────────┤
    │  boundaries     tool (+projection, platform, tooltest)          │
    │                 sandbox (+netpolicy, sessionkit, sandboxtest)   │
    ├─────────────────────────────────────────────────────────────────┤
    │  inference      inference (+route, inferencetest)               │
    ├─────────────────────────────────────────────────────────────────┤
    │  contracts      message · errdefs · model · outcome · fsx       │
    │                 skill · config · identity                       │
    ├─────────────────────────────────────────────────────────────────┤
    │  observability  telemetry (+setup, telemetrytest)               │
    └─────────────────────────────────────────────────────────────────┘
```

**Two rules keep that shape.** Imports travel downwards only: `host` reaches for
`deploy` and every contract below it, and nothing below the execution layer knows
what a conversation is — `core/host` owns the process, the deployment table and the
session, and history is an argument to a turn rather than something an engine
remembers. `core/telemetry` sits outside the ladder: every layer reports through it,
so it is above nothing and below nothing. The other rule is ownership — whoever
builds a value closes it, in reverse construction order, and a value handed in
through an option is somebody else's to close.

The implementations are siblings of `core`, not packages inside it: a provider,
backend, engine or tool source implemented outside this repository must be
importable beside the ones here, so they live in their own modules and `core`
compiles alone (`GOWORK=off`). `core/internal/layout` is the test that keeps it that
way.

`core/telemetry` sits outside the ladder: every layer reports through it, so it is
above nothing and below nothing.

## Module map

| Path                                                  | Role                                                                                              | Distribution        |
| ----------------------------------------------------- | ------------------------------------------------------------------------------------------------- | ------------------- |
| [`core`](core/)                                       | Platform module: contracts, contracts for inference/tool/sandbox, host, deploy, policy, telemetry | Versioned Go module |
| [`inference`](inference/)                             | Protocol families and provider adapters: openai, anthropic, bytedance, dashscope, minimax         | Versioned Go module |
| [`sandbox`](sandbox/)                                 | Backends (`local`, `remote`) with `netpolicy` and `sessionkit`                                    | Versioned Go module |
| [`agent`](agent/)                                     | Engines: `graph`, `a2a`, `checkpoint`                                                             | Versioned Go module |
| [`tool`](tool/)                                       | Tool sources: `builtin/*`, `mcp`, `dispatch`, `middleware`                                        | Versioned Go module |
| [`examples/forge`](examples/forge/)                   | Runnable demo: a werewolf table played by agents                                                  | Example             |
| [`examples/parallel`](examples/parallel/)             | Standalone consumer: MCP search bridge, pinned to an older `core`                                 | Example             |
| [`scripts/releasegate`](scripts/releasegate/)         | Release automation                                                                                | Tool                |
| [`skills/flowcraft-deploy`](skills/flowcraft-deploy/) | Codex skill: reference cards, templates, validator                                                | Skill               |

## Highlights

### Two documents, one implementation table

`core/deploy` owns the two document shapes and the table of implementations the
application registered — provider, backend, tool source, middleware, hook, program —
and owns no implementation itself. A document that names one nobody registered is a
load error listing what was registered, so a deployment cannot be loadable and yet
not deployable. Layers merge mapping by key, sequences are replaced whole, and
placeholders (`${VAR}`, `${scheme:arg}`) expand once after the merge;
`config.RequiredPlaceholders` prints the whole startup checklist in one pass instead
of failing at the first unset reference.

Agent documents carry no authority and no runtime section: what a session may do is
the grant its caller states at open, and what the process offers is the process
document's.

### A graph engine that can loop

`agent/graph` runs a graph as a frontier loop, so a ReAct cycle is just edges: no
topological order, no cycle rejection. A node produces state, never a value on an
edge — state keys declare their own merge rule (`single`, `last`, `append`),
fan-out shares immutable message sequences, and edges carry conditions compiled at
build time. Nodes are kinds too: `model`, `tool`, `condition`, `script` (Starlark)
and whatever the application registers.

### A boundary with two backends and one proxy

`core/sandbox` is the contract — `Runner`, `Confine`, `Enforcement` — and the
backends are packages of the `sandbox` module: `local` runs with the process user's
own authority and declares no filesystem bounds, and `remote` carries the same
`Runner` across a host boundary over HTTP or a unix socket, with cursor reads, one
SSE stream and retries only where repeating is safe. `netpolicy` is the egress
matcher the backends share, and a backend that cannot enforce a confine must refuse
it at construction instead of running unguarded.

Nothing is widened in place: a command the boundary refuses may be approved once,
and it then runs in a temporary cell built from the widened grant.

### Inference with five provider packages

`core/inference` is the contract (`Generator`, `Embedder`, `Decider`, `Catalog`),
and the `inference` module holds the protocol families and adapters: `openai`,
`anthropic`, `bytedance`, `dashscope`, `minimax`. Routing is a policy over the
declared line-up: capability-filtered candidates, per-target attempts, bounded
backoff, and a retry classification that reads the contract's error kinds rather
than strings. Providers ship no model line-up — a model exists because a process
document declared it, and a session may only call the models its grant names.

### Durable runs

`core/agent` checkpoints a task as a projection plus an append-only event log,
written in one transaction, so `Load` costs the same for a one-turn task and a
thousand-turn one. Two counters (`Version` from the store, `ServerVersion` from a
protocol adapter) let an engine and an adapter write the same task without
rejecting each other. A resume is refused rather than replayed into a stack that
never produced it: `deploy.Fingerprint` names the deployment set, and the
checkpoint's manifest names the authority the conversation actually held.

## Documentation

The canonical reference is the per-package `doc.go` files, browsable on
pkg.go.dev. Topic guides live in [`docs/guides/`](docs/guides/):

- [Graph Engine](docs/guides/graph.md) — `agent/graph`: the frontier loop,
  declared state, compiled conditions, node kinds, checkpoints and replay.
- [Tool System](docs/guides/tool.md) — `core/tool`: the function-calling
  contract, the projection, middleware, the MCP bridge and `tool_search`.
- [Agent Skills](docs/guides/skill.md) — `core/skill`: `SKILL.md` discovery, the
  shared ranking kernel, and the per-turn `## Skills` section.
- [Script Rules](docs/guides/script.md) — `agent/graph/script`: bounded rule
  scripts as a graph node, with budgets, tool policy and replay.
- [Process and Turn Runtime](docs/guides/runtime.md) — `core/host`: one process
  per document, one session per conversation, and the turn that joins them.
- [Checkpoints](docs/guides/checkpoint.md) — `core/agent`: the store contract, the
  projection and its event log, blobs, and the SQLite backend.
- [Hooks](docs/guides/hooks.md) — `agent.Hooks`: the three host boundaries of a
  turn and how a deployment declares its own hook kinds.
- [Media & Attachments](docs/guides/media.md) — the sealed part vocabulary and the
  budgets a payload meets on its way into a request.
- [Sandbox](docs/guides/sandbox.md) — the command boundary: the confine, the
  backends, and the file journal.
- [Remote Sandbox](docs/guides/sandbox-remote.md) — the same `Runner` across a
  host boundary.
- [Inference Runtime](docs/guides/inference.md) — addressing, providers, model
  declarations, routing and retry, typed extensions.
- [Deployment Assembly](docs/guides/deploy.md) — the two documents, the
  implementation table, and a loader that validates without building.
- [Delegation](docs/guides/delegation.md) — one agent handing work to another,
  with inherited child sessions and a lineage a chain keeps.

### Deployment authoring skill

[`skills/flowcraft-deploy/`](skills/flowcraft-deploy/) is a Codex skill for
writing and reviewing a FlowCraft deployment — the process document, the agent
document and the graph document — with reference cards, minimal templates, and a
structural validator.

```bash
skills/flowcraft-deploy/scripts/validate-config.sh deploy.yaml
```

> The validator in `scripts/validator` pins the `core` release its checks were
> written against. `make check-skill` builds it and runs it over the shipped
> templates, and `make check-skill-pin` fails when that pin falls behind the newest
> `core` tag, so a schema change cannot leave the skill checking a shape nobody
> writes any more.

Install it into Codex (replaces an existing copy, so remove
`~/.codex/skills/flowcraft-deploy` first if present):

```bash
python3 ~/.codex/skills/.system/skill-installer/scripts/install-skill-from-github.py \
  --repo GizClaw/flowcraft --path skills/flowcraft-deploy --ref main
```

The `skills/<skill-name>/SKILL.md` layout is also discoverable through
skills.sh (`npx skills add GizClaw/flowcraft`).

Reference material:

- [`docs/`](docs/index.md) — docs landing page (guides + migration notes).
- [pkg.go.dev/github.com/GizClaw/flowcraft/core](https://pkg.go.dev/github.com/GizClaw/flowcraft/core) — platform module.
- [pkg.go.dev/github.com/GizClaw/flowcraft/inference](https://pkg.go.dev/github.com/GizClaw/flowcraft/inference) — providers.
- [pkg.go.dev/github.com/GizClaw/flowcraft/sandbox](https://pkg.go.dev/github.com/GizClaw/flowcraft/sandbox) — backends.
- [pkg.go.dev/github.com/GizClaw/flowcraft/agent](https://pkg.go.dev/github.com/GizClaw/flowcraft/agent) — engines.
- [pkg.go.dev/github.com/GizClaw/flowcraft/tool](https://pkg.go.dev/github.com/GizClaw/flowcraft/tool) — tool sources.

## Status

The workspace is `core`, `inference`, `sandbox`, `agent`, `tool`, `test` and
`examples/forge`. Every module is release-managed and pre-1.0; API surface follows
SemVer per module, and breaking changes may ship as minor bumps until a module
reaches `v1.0.0`.

Long-term memory is a separate product line built on this platform, not a module
of it: the contracts a memory backend needs (messages, turns, tool calls,
checkpoints, hooks) are all here, and the backend lives in its own repository.

## Building from source

```bash
git clone https://github.com/GizClaw/flowcraft
cd flowcraft

make help          # list every target
make ci            # vet + test for every workspace module
make lint          # golangci-lint on the CI-gated modules
make release-check # validate changesets and the pending release plan
```

This repository is a Go workspace (`go.work`). Active members are `core`,
`inference`, `sandbox`, `agent`, `tool`, `test` and `examples/forge`; release
tooling in `scripts/releasegate` builds standalone with `GOWORK=off`, and
`make check-consumers` builds each example against its own pinned releases.

## Contributing

Issues and pull requests are welcome. Before opening a PR:

1. `make ci` should be green.
2. `make lint` and `make release-check` should be green too.
3. `gofmt -s -l core/` should print nothing.
4. Tests for new behaviour; a change without one will not merge.
5. Commit messages follow Conventional Commits with a module scope
   (`feat(core): …`, `fix(inference/minimax): …`).

Library releases are declared with immutable `.release/*.json` changesets, added
when a module is actually being tagged — a changeset is not needed on every PR.
Automation aggregates pending summaries into a Release PR; merging it gates each
planned module in dependency order (isolated tidy, build, vet, race tests) and
pushes each tag as its gate passes. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for
the contract and the coordinated-release rules.

For substantial API changes, please open an issue first.

---

## License

[MIT](LICENSE) © GizClaw
