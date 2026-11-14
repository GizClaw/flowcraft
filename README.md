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
document: which agents exist, what each runs on, and what each is allowed to do.
The platform builds that document into something you own — one `Runtime` per
agent, a session table, an HTTP surface — and leaves prompting, persistence and
your product's own policies in your hands.

Graphs are one execution engine, not a required architecture: use the packages
directly, or start from the forge demo in `examples/forge`.

## Modules

- **`core`** — the platform module: message and permission contracts, inference
  (with the OpenAI and Anthropic protocol families), tools, sandbox, the runtime,
  agent execution with the graph engine and A2A, deployment documents and kinds,
  and telemetry.
- **`driver`** — provider adapters that are not protocol families, one package
  each: `bytedance`, `dashscope`, `minimax`.
- **`examples/forge`** — a runnable local demo: a werewolf table played by a cast
  of agents, with a TUI, scripted tests and self-play.
- **`tools/releasegate`** — release automation: changeset validation, release
  planning, changelog aggregation.

Modules are independently versioned; an application adopts only what it needs.

## The unit is the deployment

Three files describe one deployment, and each is validated without being built:

| File | Says |
| --- | --- |
| `deploy.yaml` | which agents exist, each with an engine plus optionally a protocol, hooks, a delegation policy and an observability level |
| a runtime layer | what one agent runs on: the sandbox boundary, the files it may write, its providers and models, its tools and skills |
| a graph document | what the graph engine runs: state slots with merge rules, nodes, the conditions on their edges |

```yaml
version: 1
agents:
  - name: assistant
    engine:
      kind: graph                       # registered kind, not a built-in name
      settings: {path: ./graphs/assistant.yaml}
    runtime:
      layers:
        - {priority: 0, path: ./runtime.yaml}
    protocol:
      kind: a2a
      settings: {peer: {url: https://client.example}}
    delegation:
      targets: [reviewer]
      modes: [sync]
```

Everything a document can name is a **kind** — engine, protocol or hook — and a
kind the host did not register is a load error listing what it did register,
never a silently skipped block. The built-ins are `a2a` and `graph` engines, the
`a2a` protocol, and no hooks: a host's policy is the host's.

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
loader := deploy.Loader{Kinds: deploy.BuiltinKinds()}
specs, err := loader.LoadFile(ctx, "deploy.yaml")   // strict; unknown fields fail

manager := host.New(deploy.BuiltinKinds(), host.Bindings{
    Store:          store,                          // sessions survive a restart
    RuntimeOptions: []runtime.Option{runtime.WithDrivers(myDrivers...)},
    LoadOptions:    []runtime.LoadOption{runtime.WithResolver("secret", vault.Resolve)},
    Harness:        "myapp@1.0.0",
})
defer manager.Close()

if _, err := manager.Deploy(ctx, specs...); err != nil { /* … */ }

sess, ok := manager.Session("assistant", "conversation-1")
if !ok { /* not deployed */ }
for event, err := range sess.Run(ctx, message.NewTextMessage(message.RoleUser, "hello")) {
    if err != nil { break }
    render(event)                                   // text, reasoning, tool calls, usage
}
```

A driver is a `runtime.Driver{Name, Build}` pair, and the adapter that maps a
written `settings` block onto the driver's typed settings belongs to your
application — `examples/forge/internal/app/drivers.go` is a working reference.
See [`docs/guides/deploy.md`](docs/guides/deploy.md) and
[`docs/guides/runtime.md`](docs/guides/runtime.md).

## Architecture

`core` is layered: a package imports only from the layers below it.

```text
                 ┌──────────────────────────────────────────┐
                 │            your application              │
                 │  drivers · secret resolvers · approvals   │
                 └───────────────────┬──────────────────────┘
                                     ▼
        ┌────────────────────────────────────────────────────────┐
        │ deployment   core/deploy (documents, kinds)            │
        │              core/agent-host (process, sessions)       │
        ├────────────────────────────────────────────────────────┤
        │ assembly     core/runtime (provider · tools · sandbox  │
        │              files · skills · journal; owns them)      │
        ├────────────────────────────────────────────────────────┤
        │ execution    core/agent · agent-graph · agent-a2a      │
        │              core/delegation                           │
        ├────────────────────────────────────────────────────────┤
        │ boundaries   core/tool (+engine, middleware, mcp)      │
        │              core/sandbox (+backends, journal, remote)  │
        ├────────────────────────────────────────────────────────┤
        │ inference    core/inference (+openai, +anthropic)       │
        │              driver/*                                  │
        ├────────────────────────────────────────────────────────┤
        │ contracts    message · errdefs · permission · fsx       │
        │              skill · search                            │
        └────────────────────────────────────────────────────────┘
```

**Two rules keep that shape.** A `Runtime` owns what it built and closes it in
reverse construction order; a value handed in through an option is somebody
else's to close. And nothing below the assembly layer knows about a conversation:
`core/agent` owns runs, `core/runtime` owns the substrate, and history is an
argument to a turn.

## Module map

| Path | Role | Distribution |
| --- | --- | --- |
| [`core`](core/) | Platform module: contracts, inference, tools, sandbox, runtime, execution, deployment | Versioned Go module |
| [`driver`](driver/) | Provider adapters that are not protocol families | Versioned Go module |
| [`examples/forge`](examples/forge/) | Runnable demo: a werewolf table played by agents | Example |
| [`tools/releasegate`](tools/releasegate/) | Release automation | Tool |
| [`skills/flowcraft-config`](skills/flowcraft-config/) | Codex skill: reference cards, templates, validator | Skill |

## Highlights

### One document, three kind tables

A deployment names agents; each agent names an engine, optionally a protocol and
its own hooks. Registration lives with the loader (`deploy.BuiltinKinds`,
`WithEngines`/`WithProtocols`/`WithHooks`) while the implementation stays in its
own package, so a kind cannot be loadable and yet not deployable. Placeholders
(`${VAR}`, `${VAR:-fallback}`, `${scheme:arg}`) expand once, after the layers
merge, and `Loader.RequiredPlaceholders` prints the whole startup checklist in
one pass instead of failing at the first unset reference.

### A graph engine that can loop

`core/agent-graph` runs a graph as a frontier loop, so a ReAct cycle is just
edges: no topological order, no cycle rejection. A node produces state, never a
value on an edge — state keys declare their own merge rule (`single`, `last`,
`append`), fan-out shares immutable message sequences, and edges carry conditions
compiled at build time. Nodes are kinds too: `model`, `tool`, `condition` and
whatever the host registers.

### A boundary with four backends and one proxy

`core/sandbox` is the contract, `core/sandbox/engine` is the policy layer
(defaults, allow-list, approval, escalation, the safe-read-only classifier), and
the backends are siblings: `local`, `bwrap` (Linux namespaces), `seatbelt`
(macOS), `windows` (job objects, optional Low-integrity confinement). Every call
can narrow the boundary it was given and never widen it; a cap a backend cannot
enforce fails the build instead of being ignored. `core/sandbox-remote` runs the
same `Runner` on another machine — HTTP or unix socket, cursor reads, one SSE
stream, retries only where repeating is safe.

### Inference with two protocol families

`core/inference` is the contract (`Generator`, `Embedder`, `Decider`, `Catalog`),
`core/inference-openai` and `-anthropic` are the protocol families, and
`driver/*` holds the vendors that are not one. Routing is a policy over the
declared line-up: capability-filtered candidates, per-target attempts, bounded
backoff, and a retry classification that reads the contract's error kinds rather
than strings. Drivers ship no model line-up — a model exists because a deployment
declared it, and its limits are enforced before the provider is opened.

### Durable runs

`core/agent` checkpoints a task as a projection plus an append-only event log,
written in one transaction, so `Load` costs the same for a one-turn task and a
thousand-turn one. Two counters (`Version` from the store, `ServerVersion` from a
protocol adapter) let an engine and an adapter write the same task without
rejecting each other, and a changed deployment invalidates a resume through the
fingerprint rather than replaying a transcript into a stack that never produced
it.

## Documentation

The canonical reference is the per-package `doc.go` files, browsable on
pkg.go.dev. Topic guides live in [`docs/guides/`](docs/guides/):

- [Graph Engine](docs/guides/graph.md) — `core/agent-graph`: the frontier loop,
  declared state, compiled conditions, node kinds, checkpoints and replay.
- [Tool System](docs/guides/tool.md) — `core/tool`: the function-calling
  contract, the projection, middleware, the MCP bridge and `tool_search`.
- [Agent Skills](docs/guides/skill.md) — `core/skill`: `SKILL.md` discovery, the
  shared ranking kernel, and the per-turn `## Skills` section.
- [Script Rules](docs/guides/script.md) — `core/agent-graph/script`: bounded rule
  scripts as a graph node, with budgets, tool policy and replay.
- [Application Runtime](docs/guides/runtime.md) — `core/runtime`: one runtime per
  document — provider, tools, sandbox, files, skills, journal.
- [Checkpoints](docs/guides/checkpoint.md) — `core/agent`: the store contract, the
  projection and its event log, blobs, and the SQLite backend.
- [Hooks](docs/guides/hooks.md) — `agent.Hooks`: the three host boundaries of a
  turn and how a deployment declares its own hook kinds.
- [Media & Attachments](docs/guides/media.md) — the sealed part vocabulary and the
  budgets a payload meets on its way into a request.
- [Sandbox](docs/guides/sandbox.md) — the command boundary: per-call permission,
  backends, escalation, and the file journal.
- [Remote Sandbox](docs/guides/sandbox-remote.md) — the same `Runner` across a
  host boundary.
- [Inference Runtime](docs/guides/inference.md) — addressing, drivers, model
  declarations, routing and retry, typed extensions.
- [Deployment Assembly](docs/guides/deploy.md) — the deployment document, the kind
  tables, and a loader that validates without building.
- [Delegation](docs/guides/delegation.md) — one agent handing work to another,
  with derived child sessions and a lineage a chain keeps.

### Configuration authoring skill

[`skills/flowcraft-config/`](skills/flowcraft-config/) is a Codex skill for
writing and reviewing FlowCraft configuration: reference cards, minimal
templates, and a structural validator.

```bash
skills/flowcraft-config/scripts/validate-config.sh deploy.yaml
```

> The cards and templates in the skill still describe the pre-rebuild
> configuration model, and the validator pins an older `core` release. They are
> being rebuilt; until then the guides above are the reference.

Install it into Codex (replaces an existing copy, so remove
`~/.codex/skills/flowcraft-config` first if present):

```bash
python3 ~/.codex/skills/.system/skill-installer/scripts/install-skill-from-github.py \
  --repo GizClaw/flowcraft --path skills/flowcraft-config --ref main
```

The `skills/<skill-name>/SKILL.md` layout is also discoverable through
skills.sh (`npx skills add GizClaw/flowcraft`).

Reference material:

- [`docs/`](docs/index.md) — docs landing page (guides + migration notes).
- [pkg.go.dev/github.com/GizClaw/flowcraft/core](https://pkg.go.dev/github.com/GizClaw/flowcraft/core) — platform module.
- [pkg.go.dev/github.com/GizClaw/flowcraft/driver](https://pkg.go.dev/github.com/GizClaw/flowcraft/driver) — provider adapters.

## Status

The workspace is `core`, `driver` and `examples/forge`. `core` and `driver` are
release-managed and pre-1.0; API surface follows SemVer per module, and breaking
changes may ship as minor bumps until a module reaches `v1.0.0`.

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
`driver` and `examples/forge`; release tooling in `tools/releasegate` builds
standalone with `GOWORK=off`, and `make check-consumers` builds each example
against its own pinned releases.

## Contributing

Issues and pull requests are welcome. Before opening a PR:

1. `make ci` should be green.
2. `make lint` and `make release-check` should be green too.
3. `gofmt -s -l core/` should print nothing.
4. Tests for new behaviour; a change without one will not merge.
5. Commit messages follow Conventional Commits with a module scope
   (`feat(core/runtime): …`, `fix(driver/minimax): …`).

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
