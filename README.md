<div align="center">

# FlowCraft

**Production-grade Go SDK for building AI agents with long-term memory, knowledge retrieval, and voice — runnable as a library, a daemon, or a real-time pipeline.**

[![CI](https://github.com/GizClaw/flowcraft/actions/workflows/ci.yml/badge.svg)](https://github.com/GizClaw/flowcraft/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/GizClaw/flowcraft/sdk.svg)](https://pkg.go.dev/github.com/GizClaw/flowcraft/sdk)
[![Go Report Card](https://goreportcard.com/badge/github.com/GizClaw/flowcraft/sdk)](https://goreportcard.com/report/github.com/GizClaw/flowcraft/sdk)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](https://go.dev/dl/)

</div>

---

FlowCraft is a layered, **batteries-included** toolkit for shipping LLM applications in Go. Pick the layer you need:

- **`sdk`** — Composable primitives: agents, DAG executor, conversation history, hybrid retrieval, knowledge bases, kanban-style multi-agent delegation.
- **`sdkx`** — Drop-in providers: OpenAI, Anthropic, DeepSeek, MiniMax, ByteDance / Volcengine, plus embedding + reranker backends.
- **`vessel`** — In-process runtime that hosts your agents with proper lifecycle (Submit / Drain / Stop), restart policies, probes, sidecars, and shared history.
- **`vesseld`** — A standalone daemon that runs `vessel` instances from declarative YAML, exposes an HTTP + SSE control plane, and shares LLM clients & rate limits across many vessels.
- **`voice`** — Real-time STT → LLM → TTS pipeline with VAD, barge-in, and WebRTC.

Everything ships as Go modules with semantic versioning — depend on what you need, ignore the rest.

---

## Why FlowCraft

| You want…                                                    | FlowCraft gives you…                                                                                                      |
| ------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------- |
| Strict separation between **engine** and **agent**           | `sdk/engine` is a leaf package; `sdk/agent` orchestrates above it. No "framework is the runtime" coupling.                |
| **Long-term memory** that actually retrieves what's relevant | `sdk/recall` ships hybrid BM25 + vector retrieval with predicate-alias normalisation, not just embedding similarity.      |
| **Multi-agent collaboration** without a graph DSL            | `sdk/kanban` exposes any agent as a tool to any other agent — composition is just function calls.                         |
| **A daemon you can deploy**                                  | `vesseld` is a single static binary: `vesseld start --config ./config -R`. No runtime, no Python, no Docker required.     |
| **Voice agents** that don't reinvent VAD                     | `voice/` ships VAD, endpointing, barge-in, WebRTC — wire it to any STT/TTS provider in `sdkx`.                            |
| **Provider portability**                                     | The same agent code runs against OpenAI, Anthropic, DeepSeek, MiniMax, or Volcengine — switch by changing one YAML field. |

---

## Quickstart

### Library — a 20-line agent

```bash
go get github.com/GizClaw/flowcraft/sdk@latest
go get github.com/GizClaw/flowcraft/sdkx@latest
```

```go
package main

import (
 "context"
 "fmt"

 "github.com/GizClaw/flowcraft/sdk/agent"
 "github.com/GizClaw/flowcraft/sdk/graph/runner"
 "github.com/GizClaw/flowcraft/sdk/llm"
 "github.com/GizClaw/flowcraft/sdk/model"
 "github.com/GizClaw/flowcraft/sdkx/openai"
)

func main() {
 llm.Register("gpt", openai.New("gpt-4o-mini"))

 a := &agent.Agent{
  ID:   "hello",
  Card: agent.Card{Name: "Hello", System: "You are a friendly greeter."},
 }

 res, _ := agent.Run(context.Background(), a, runner.New(),
  agent.Request{Message: model.UserText("Greet me in one sentence.")})

 fmt.Println(res.Messages[len(res.Messages)-1].Text())
}
```

### Daemon — declarative multi-vessel deployment

```bash
go install github.com/GizClaw/flowcraft/cmd/vesseld@latest

# One daemon, two independently configured vessels, sharing one OpenAI client.
vesseld validate --config examples/vesseld-multi-vessel -R
vesseld start    --config examples/vesseld-multi-vessel -R
```

```bash
# Synchronous call:
curl --unix-socket /tmp/vesseld.sock -X POST http://vesseld/v1/vessels/support/call \
  -H 'content-type: application/json' \
  -d '{"agent":"support-agent","query":"What are your business hours?"}'

# Async submit + SSE log tail:
RUN=$(curl -s --unix-socket /tmp/vesseld.sock -X POST http://vesseld/v1/vessels/triage/submit \
  -H 'content-type: application/json' \
  -d '{"agent":"triage-dispatcher","query":"My order is two weeks late."}' | jq -r .run_id)

curl --unix-socket /tmp/vesseld.sock "http://vesseld/v1/vessels/triage/logs?run_id=$RUN"
```

See [`examples/vesseld-multi-vessel/`](examples/vesseld-multi-vessel/) for the full walkthrough.

### Voice — STT/LLM/TTS with one struct

```go
p := voice.NewPipeline(voice.Config{
    STT: deepgram.New(...),
    LLM: openai.New("gpt-4o-mini"),
    TTS: elevenlabs.New(...),
    VAD: detect.NewWebRTC(),
})
p.Run(ctx, micStream, speakerStream)
```

---

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                       Your Application                           │
└─────────────┬─────────────────────────────────────┬──────────────┘
              │                                     │
       ┌──────▼──────┐                       ┌──────▼──────┐
       │  vesseld    │  ── HTTP + SSE ──┐    │   voice/    │  WebRTC / WS
       │   (daemon)  │                  │    │ (pipeline)  │
       └──────┬──────┘                  │    └──────┬──────┘
              │                         │           │
       ┌──────▼─────────────────────────▼───────────▼────────┐
       │                      vessel/                        │
       │   Captain ▸ Submit / Drain / Stop / Restart         │
       │   Multi-Agent + Kanban + Sidecars + Shared History  │
       └──────────────────────┬──────────────────────────────┘
                              │
       ┌──────────────────────▼──────────────────────────────┐
       │                       sdk/                          │
       │   agent ▸ engine ▸ graph                            │
       │   recall ▸ history ▸ knowledge ▸ kanban ▸ tool      │
       │   model ▸ llm ▸ event ▸ workspace ▸ telemetry       │
       └──────────────────────┬──────────────────────────────┘
                              │
       ┌──────────────────────▼──────────────────────────────┐
       │                      sdkx/                          │
       │   openai • anthropic • deepseek • minimax • ...     │
       │   embedding • retrieval • reranker                  │
       └─────────────────────────────────────────────────────┘
```

**Constraint**: `sdk/engine` is a leaf — it does NOT import `agent`, `graph`, `history`, `recall`, `llm`, `tool`, or `workflow`. New execution engines plug in by implementing `engine.Engine` against the `Host` capability interface.

---

## Module map

| Module                                     | What it gives you                                                                                          | Stable        |
| ------------------------------------------ | ---------------------------------------------------------------------------------------------------------- | ------------- |
| [`sdk`](sdk/)                              | Core primitives — agent, graph DAG, recall, history, knowledge, kanban                                     | yes           |
| [`sdkx`](sdkx/)                            | Provider implementations (OpenAI, Anthropic, DeepSeek, MiniMax, Volcengine) + retrieval/embedding adapters | yes           |
| [`vessel`](vessel/)                        | In-process agent runtime — Captain, restart, probes, sidecars                                              | `v0.1.0-rc.2` |
| [`cmd/vesseld`](cmd/vesseld/)              | Standalone daemon binary — declarative YAML, HTTP/SSE control plane                                        | `v0.1.0-rc.1` |
| [`voice`](voice/)                          | Real-time voice pipeline (VAD / STT / LLM / TTS / WebRTC)                                                  | yes           |
| [`examples/`](examples/)                   | Worked end-to-end examples (voice pipeline, multi-vessel daemon, …)                                        | —             |
| [`tests/quality/`](tests/quality/)         | Quality / regression suites (knowledge retrieval, vessel runtime)                                          | —             |
| [`tests/e2e/`](tests/e2e/)                 | Black-box end-to-end suites (vesseld subprocess)                                                           | —             |
| [`tests/conformance/`](tests/conformance/) | Provider conformance — same surface, every backend                                                         | —             |

---

## Highlights

### Hybrid memory that actually recalls (`sdk/recall`)

- BM25 lexical + vector semantic, combined via Reciprocal Rank Fusion.
- Predicate alias normalisation so "favourite color" and "favorite colour" hit the same memory.
- Pluggable persistence (in-memory, SQLite, Postgres + pgvector).

### Streaming, durable, resumable (`sdk/engine`)

- `Subject`-routed event bus — every step emits structured envelopes.
- `Checkpoint` / `CheckpointStore` contract — pause and resume an agent across restarts.
- `Interrupt` / `Wait` semantics that compose cleanly with `context.Context`.

### Production-shaped runtime (`vessel` + `vesseld`)

- Declarative YAML — vessels, agents, engines, history, sidecars, probes, restart policies.
- `Handle.OnTerminate` hooks for synchronous bookkeeping (registry, OTel spans, metrics).
- Rate limits and concurrency caps shared across vessels via the daemon-wide gate.
- SSE log streaming for every run with replay-friendly delta envelopes.

### Voice without the duct tape (`voice`)

- VAD with hysteresis, endpointing, barge-in.
- WebRTC ingress / egress.
- Provider-agnostic: any `sdkx` STT/TTS backend works.

---

## Documentation

The canonical reference is the per-package `doc.go` files, browsable on pkg.go.dev:

- [pkg.go.dev/github.com/GizClaw/flowcraft/sdk](https://pkg.go.dev/github.com/GizClaw/flowcraft/sdk) — core primitives (agent, engine, recall, history, knowledge, …)
- [pkg.go.dev/github.com/GizClaw/flowcraft/sdkx](https://pkg.go.dev/github.com/GizClaw/flowcraft/sdkx) — provider implementations
- [pkg.go.dev/github.com/GizClaw/flowcraft/vessel](https://pkg.go.dev/github.com/GizClaw/flowcraft/vessel) — runtime layer
- [pkg.go.dev/github.com/GizClaw/flowcraft/voice](https://pkg.go.dev/github.com/GizClaw/flowcraft/voice) — voice pipeline

Worked examples live under [`examples/`](examples/) — each one is runnable end-to-end with a single command.

---

## Status

`sdk` and `sdkx` are stable and released continuously. `vessel` and `cmd/vesseld` are at `v0.1.0-rc.*` and stabilising. The next milestone (`v0.2`) covers durable execution, MCP support, OTel exporters, and an evaluation harness.

API surface is governed by SemVer per module. Breaking changes ship as minor bumps until each module reaches `v1.0.0`.

---

## Building from source

```bash
git clone https://github.com/GizClaw/flowcraft
cd flowcraft

make help          # list every target
make ci            # vet + test for all in-tree modules
make test-e2e      # black-box vesseld suite (no API key required)
```

This repo is a Go workspace (`go.work`). The in-tree modules are `sdk`, `sdkx`, `vessel`, `voice`, `cmd/vesseld`, and `tests/quality/vessel`. The off-workspace modules (`bench`, `examples/voice-pipeline`, `tests/conformance`, `tests/quality/knowledge`, `tests/e2e/vesseld`) pin released versions and run with `GOWORK=off`.

---

## Contributing

Issues and pull requests are welcome. Before opening a PR:

1. `make ci` should be green.
2. `gofmt -l .` should print nothing.
3. Tests for new features. New behaviour without a test won't merge.
4. Commit messages follow Conventional Commits (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`).

For larger work, please open a discussion or draft RFC issue first — it's much faster than reviewing a 5k-line PR cold.

---

## License

[MIT](LICENSE) © GizClaw
