<div align="center">

# FlowCraft

**A modular Go toolkit for extensible AI applications, long-term memory, provider integrations, voice, and local interactive workflows.**

[![CI](https://github.com/GizClaw/flowcraft/actions/workflows/ci.yml/badge.svg)](https://github.com/GizClaw/flowcraft/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/GizClaw/flowcraft/sdk.svg)](https://pkg.go.dev/github.com/GizClaw/flowcraft/sdk)
[![Go Report Card](https://goreportcard.com/badge/github.com/GizClaw/flowcraft/sdk)](https://goreportcard.com/report/github.com/GizClaw/flowcraft/sdk)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](https://go.dev/dl/)

</div>

---

FlowCraft is a Go workspace for building and evaluating AI applications without
tying application code to one model provider or execution model. Graphs are one
built-in option, not a required architecture. Use the SDK packages directly, or
start with Claw for a runnable local workspace.

- **`cmd/claw`** — Local CLI and TUI for creating agent workspaces, running
  conversations, inspecting memory, serving debug APIs, and executing scripted
  tests.
- **`sdk`** — Agent, graph, engine, model, tool, event, telemetry, and workspace
  contracts.
- **`memory`** — Recall, conversation history, knowledge retrieval, text
  processing, and persistence backends.
- **`sdkx`** — Provider adapters and optional implementations for LLMs,
  embeddings, reranking, checkpointing, sandboxing, and Claw.
- **`voice`** — Real-time STT → LLM → TTS pipelines with VAD, barge-in, and
  WebRTC.

The library layers are independently versioned Go modules. Applications can
adopt only the layers they need.

---

## Why FlowCraft

| You need…                          | FlowCraft provides…                                                                                                |
| ---------------------------------- | ------------------------------------------------------------------------------------------------------------------ |
| A runnable local agent environment | Claw workspaces, TUI sessions, debug HTTP endpoints, and scripted tests                                            |
| Explicit execution contracts       | A small `engine.Engine` / `engine.Host` boundary that supports graph, scripted, remote, or custom execution models |
| Long-term memory                   | Hybrid recall, histories, knowledge stores, retrieval indexes, and SQLite/Postgres backends                        |
| Provider portability               | OpenAI, Anthropic, DeepSeek, MiniMax, and Volcengine adapters behind shared SDK interfaces                         |
| Multi-agent composition            | Kanban agent-as-tool delegation that can run over graph or custom engines                                          |
| Real-time voice                    | VAD, endpointing, barge-in, STT/TTS contracts, and WebRTC integration                                              |

---

## Quickstart

### Run a local agent with Claw

Claw is the fastest way to explore the stack. It creates persistent local
workspaces, opens an interactive TUI, exposes debug endpoints, and runs scripted
agent tests.

```bash
cd cmd/claw
go run . help

# Open the interactive TUI and select an embedded raid config.
go run . tui new
```

Build a reusable local binary:

```bash
cd cmd/claw
go build -o claw .
./claw help
```

See [`cmd/claw/README.md`](cmd/claw/README.md) for workspace, TUI, debug API,
configuration, and test commands.

### Embed FlowCraft in a Go service

Use `sdk` directly and add `memory` or `sdkx` when the application needs recall,
history, knowledge, persistence, or provider adapters. Applications execute any
`engine.Engine` through `agent.Run`; the bundled graph runner is one available
implementation, alongside scripted, remote, or application-defined engines.

- [`sdk/agent/run_test.go`](sdk/agent/run_test.go) — minimal `agent.Run` patterns
- [`examples/voice-pipeline/setup.go`](examples/voice-pipeline/setup.go) — a real graph-runner build wiring an LLM provider + script node

### Voice — STT → LLM → TTS

```go
p := voice.NewPipeline(
    sttProvider,                 // any voice/stt backend (e.g. bytedance, …)
    ttsProvider,                 // any voice/tts backend (e.g. minimax, …)
    eng,                         // engine.Engine driving each turn
    agent.Agent{ID: "voice"},
    voice.WithSTTOptions(stt.WithLanguage("zh"), stt.WithTargetSampleRate(16000)),
    voice.WithTTSOptions(tts.WithCodec(audio.CodecMP3)),
)
```

End-to-end: [`examples/voice-pipeline/`](examples/voice-pipeline/) — a runnable WebRTC voice agent.

---

## Architecture

The SDK defines the execution contracts. Memory, provider adapters, voice, and
Claw compose those contracts without becoming dependencies of the core.

```
                    ┌──────────────────────┐
                    │   Your application   │
                    └──────────┬───────────┘
                               │
         ┌─────────────────────┼─────────────────────┐
         │                     │                     │
  ┌──────▼──────┐       ┌──────▼──────┐       ┌──────▼──────┐
  │  cmd/claw   │       │   voice/    │       │    sdkx/    │
  │ CLI · TUI · │       │ STT · TTS · │       │ providers · │
  │ debug · test│       │ VAD · WebRTC│       │ persistence │
  └──────┬──────┘       └──────┬──────┘       └──────┬──────┘
         │                     │                     │
         └──────────────┬──────┴──────────────┬──────┘
                        │                     │
                 ┌──────▼──────┐       ┌──────▼───────┐
                 │   memory/   │──────►│    sdk/      │
                 │ recall ·    │       │ agent · graph│
                 │ history ·   │       │ engine · tool│
                 │ knowledge   │       │ event · model│
                 └─────────────┘       └──────────────┘
```

**Layering rule:** `sdk/engine` is a leaf package. It does not import agent,
graph, LLM, or tool packages. Execution implementations satisfy
`engine.Engine`; hosts provide capabilities through `engine.Host`. Memory lives
in its own module and depends on SDK contracts, never the reverse.

---

## Module map

| Path                                      | Role                                                                           | Distribution         |
| ----------------------------------------- | ------------------------------------------------------------------------------ | -------------------- |
| [`cmd/claw`](cmd/claw/)                   | Local workspace runner, TUI, debug API, and scripted-test CLI                  | Source-built command |
| [`sdk`](sdk/)                             | Agent execution contracts, graph runtime, tools, models, events, and telemetry | Versioned Go module  |
| [`memory`](memory/)                       | Recall, history, knowledge, retrieval, text processing, and stores             | Versioned Go module  |
| [`sdkx`](sdkx/)                           | Provider, persistence, sandbox, and application adapters                       | Versioned Go module  |
| [`voice`](voice/)                         | Real-time STT, TTS, VAD, audio, and WebRTC pipeline                            | Versioned Go module  |
| [`eval`](eval/)                           | Offline and synthetic quality-evaluation harnesses                             | Workspace module     |
| [`examples`](examples/)                   | Runnable recall-chatbot and voice-pipeline integrations                        | Examples             |
| [`tests/conformance`](tests/conformance/) | Provider conformance suites                                                    | Tests                |
| [`tests/e2e`](tests/e2e/)                 | Retrieval end-to-end coverage                                                  | Tests                |

---

## Highlights

### Local workflows with Claw (`cmd/claw`, `sdkx/claw`)

- Persistent workspaces with config, history, memory, and graph state.
- Interactive TUI plus an embeddable Go runtime.
- Debug HTTP endpoints for workspace, history, memory, and recall inspection.
- Scripted and simulation-style tests with captured workspace artifacts.

### Hybrid memory that actually recalls (`memory/recall`)

- Three-lane retrieval (BM25 + vector + entity), fused via **Reciprocal Rank Fusion** (K=60), then re-weighted by entity-overlap boost, supersede decay, and time decay.
- Predicate alias normalisation so "favourite color" and "favorite colour" hit the same memory.
- Pluggable `retrieval.Index` backend — `memory/retrieval/memory` (in-memory), `memory/retrieval/sqlite` (SQLite), and `memory/retrieval/postgres` (Postgres + pgvector) ship in-tree; bring your own by implementing `retrieval.Index`.

### Streaming, durable, resumable (`sdk/engine`)

- `Subject`-routed event bus — every step emits structured envelopes.
- `Checkpoint` / `CheckpointStore` contract — pause and resume an agent across restarts.
- `Interrupt` / `Wait` semantics that compose cleanly with `context.Context`.

### Voice without the duct tape (`voice`)

- VAD with hysteresis, endpointing, barge-in.
- WebRTC ingress / egress.
- Provider-agnostic: any `sdkx` STT/TTS backend works.

---

## Documentation

The canonical reference is the per-package `doc.go` files, browsable on pkg.go.dev:

- [`cmd/claw/README.md`](cmd/claw/README.md) — CLI, TUI, workspace, debug API, and scripted-test guide
- [pkg.go.dev/github.com/GizClaw/flowcraft/sdk](https://pkg.go.dev/github.com/GizClaw/flowcraft/sdk) — core primitives (agent, engine, graph, llm, tool, telemetry, …)
- [pkg.go.dev/github.com/GizClaw/flowcraft/memory](https://pkg.go.dev/github.com/GizClaw/flowcraft/memory) — recall, history, knowledge, retrieval, and text packages
- [pkg.go.dev/github.com/GizClaw/flowcraft/sdkx](https://pkg.go.dev/github.com/GizClaw/flowcraft/sdkx) — provider implementations
- [pkg.go.dev/github.com/GizClaw/flowcraft/voice](https://pkg.go.dev/github.com/GizClaw/flowcraft/voice) — voice pipeline

Worked examples live under [`examples/`](examples/) — each one is runnable end-to-end with a single command.

---

## Status

The active project surface is `sdk`, `memory`, `sdkx`, `voice`, and Claw.
Library modules are released independently and remain pre-1.0. Claw currently
ships from source as the local interactive runner.

Durable execution contracts, Postgres and SQLite checkpoint stores, OTel
instrumentation, quality-evaluation harnesses, and retrieval end-to-end coverage
are maintained in-tree.

The next milestone is the assertion-graph memory model: first-class observations, assertions, and links with provenance, so recall can retrieve linked evidence packets instead of isolated facts.

API surface is governed by SemVer per module. Breaking changes may ship as minor
bumps until a module reaches `v1.0.0`.

---

## Building from source

```bash
git clone https://github.com/GizClaw/flowcraft
cd flowcraft

make help          # list every target
make ci            # vet + test for all in-tree modules
make test-e2e      # build-tagged retrieval end-to-end suite
make release-check # validate changesets and the pending release plan
```

This repository is a Go workspace. Active members are `sdk`, `memory`, `sdkx`,
`voice`, `cmd/claw`, and `eval`. Some examples and test harnesses intentionally
run with `GOWORK=off` against pinned released modules.

---

## Contributing

Issues and pull requests are welcome. Before opening a PR:

1. `make ci` should be green.
2. `gofmt -l .` should print nothing.
3. Tests for new features. New behaviour without a test won't merge.
4. Commit messages follow Conventional Commits (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`).

Library releases are declared explicitly with immutable `.release/*.json`
changesets for `sdk`, `memory`, `sdkx`, and `voice`; a changeset is optional for
ordinary PRs. After merge, automation aggregates pending summaries into a
Release PR that updates `CHANGELOG.md`. Merging that PR runs isolated tidy,
build, vet, and race-test gates before all planned tags are pushed atomically.
Claw remains source-only and does not receive module tags. See
[`CONTRIBUTING.md`](CONTRIBUTING.md) for the contract and coordinated dependency
rules.

For larger work, please open a discussion or draft RFC issue first — it's much faster than reviewing a 5k-line PR cold.

---

## License

[MIT](LICENSE) © GizClaw
