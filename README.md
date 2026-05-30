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

- **`sdk`** — Composable primitives: agents, DAG executor, LLM contracts, tools, telemetry, workspaces, and kanban-style multi-agent delegation.
- **`memory`** — Long-term recall, conversation history, knowledge retrieval, text processing, and memory-backed stores.
- **`sdkx`** — Drop-in providers: OpenAI, Anthropic, DeepSeek, MiniMax, ByteDance / Volcengine, plus embedding, reranker, sandbox, and compatibility adapters.
- **`vessel`** — In-process runtime that hosts your agents with lifecycle management, restart policies, probes, sidecars, per-run workspaces, and assembly helpers.
- **`vesseld`** — A standalone daemon that runs `vessel` instances from declarative YAML, exposes an HTTP + SSE control plane, and shares LLM clients & rate limits across many vessels.
- **`voice`** — Real-time STT → LLM → TTS pipeline with VAD, barge-in, and WebRTC.

Everything ships as Go modules with semantic versioning — depend on what you need, ignore the rest.

---

## Why FlowCraft

| You want…                                                    | FlowCraft gives you…                                                                                                      |
| ------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------- |
| Strict separation between **engine** and **agent**           | `sdk/engine` is a leaf package; `sdk/agent` orchestrates above it. No "framework is the runtime" coupling.                |
| **Long-term memory** that actually retrieves what's relevant | `memory/recall` ships hybrid BM25 + vector retrieval with predicate-alias normalisation, not just embedding similarity.   |
| **Multi-agent collaboration** without a graph DSL            | `sdk/kanban` exposes any agent as a tool to any other agent — composition is just function calls.                         |
| **A daemon you can deploy**                                  | `vesseld` is a single static binary: `vesseld run --config ./config -R`. No runtime, no Python, no Docker required.       |
| **Voice agents** that don't reinvent VAD                     | `voice/` ships VAD, endpointing, barge-in, WebRTC — wire it to any STT/TTS provider in `sdkx`.                            |
| **Provider portability**                                     | The same agent code runs against OpenAI, Anthropic, DeepSeek, MiniMax, or Volcengine — switch by changing one YAML field. |

---

## Quickstart

### Daemon — declarative multi-vessel deployment

The fastest way to ship something runnable: write YAML, point `vesseld` at it.

```bash
go install github.com/GizClaw/flowcraft/cmd/vesseld@latest

# One daemon, two independently configured vessels, sharing one OpenAI client.
vesseld validate --config examples/vesseld-multi-vessel -R
vesseld run      --config examples/vesseld-multi-vessel -R
```

```bash
SOCK=/tmp/vesseld-multi-vessel.sock   # set in examples/vesseld-multi-vessel/daemon.yaml

# Synchronous call (waits for completion):
curl --unix-socket $SOCK -X POST http://vesseld/v1/vessels/support/call \
  -H 'content-type: application/json' \
  -d '{"agent":"support-agent","query":"What are your business hours?"}'

# Async submit + SSE log tail:
RUN=$(curl -s --unix-socket $SOCK -X POST http://vesseld/v1/vessels/triage/submit \
  -H 'content-type: application/json' \
  -d '{"agent":"triage-dispatcher","query":"My order is two weeks late."}' | jq -r .run_id)

curl --unix-socket $SOCK "http://vesseld/v1/vessels/triage/logs?run_id=$RUN"
```

**Remote access via TCP + bearer token.** Set `spec.control.listen` and a `tokenFile` in `daemon.yaml`; validation refuses to start a TCP listener without auth:

```yaml
spec:
  control:
    socket: /tmp/vesseld-multi-vessel.sock   # local debugging stays available
    listen: 0.0.0.0:8443                     # remote access
    auth:
      tokenFile: /etc/vesseld/token           # one line: the bearer token
```

```bash
TOKEN=$(cat /etc/vesseld/token)
curl -H "Authorization: Bearer $TOKEN" http://localhost:8443/v1/vessels/support/call \
  -H 'content-type: application/json' \
  -d '{"agent":"support-agent","query":"hello"}'
```

mTLS support is available through the daemon TLS config; bearer-token TCP auth remains the lightest remote-access path for simple deployments.

See [`examples/vesseld-multi-vessel/`](examples/vesseld-multi-vessel/) for the multi-agent + Kanban delegation walkthrough, and [`examples/vesseld-with-history/`](examples/vesseld-with-history/) for an agent that remembers earlier turns of the same conversation.

### Library — programmatic SDK usage

For embedding agents directly into a Go service (no daemon), use `sdk` directly and add `memory` / `sdkx` when you need recall, history, knowledge, or provider adapters. The minimum viable wiring is a graph DAG (`graph.GraphDefinition` + `node.Factory` with `llmnode.Register`) driven by `agent.Run`. See:

- [`sdk/agent/run_test.go`](sdk/agent/run_test.go) — minimal `agent.Run` patterns
- [`tests/quality/vessel/`](tests/quality/vessel/) — full integration examples (history, sidecars, kanban)
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

Layered bottom-up. `sdk` is the foundation; `memory` builds on it, and upper layers compose the published `sdk` + `memory` surfaces.

```
   ┌──────────────────────────────────────────────────────────────┐
   │                      Your Application                        │
   └────────────┬───────────────────────────────────┬─────────────┘
                │                                   │
         ┌──────▼──────┐                            │
         │   vesseld   │ ── HTTP + SSE control ──   │
         │   (daemon)  │                            │
         └──────┬──────┘                            │
                │ composes vessel + sdkx            │
   ┌────────────┼─────────────────┐          ┌──────▼─────┐
   │     ┌──────▼───────┐  ┌──────▼──────┐   │   voice/   │  WebRTC
   │     │   vessel/    │  │    sdkx/    │   │ (pipeline) │
   │     │ runtime +    │  │ providers + │   └─────┬──────┘
   │     │ assembly     │  │ sandbox     │         │
   │     └──────┬───────┘  └──────┬──────┘         │
   │            │                 │                │
   │            ├─────────┬───────┴────────────────┘
   │            │         │
   │     ┌──────▼──────┐  │        ┌────────────────┐
   │     │   memory/   │  └───────►│      sdk/      │
   │     │ recall ·    │           │ agent · engine │
   │     │ history ·   │           │ graph · llm ·  │
   │     │ knowledge · │           │ tool · event · │
   │     │ retrieval   │           │ telemetry      │
   │     └──────┬──────┘           └────────────────┘
   │            │                         ▲
   └────────────┴─────────────────────────┘
        sdk is the foundation. memory depends on sdk; sdkx and vessel
        consume the published sdk + memory surfaces.
```

**Layering rule**: `sdk/engine` is a leaf inside `sdk/` — it does NOT import `agent`, `graph`, `llm`, `tool`, or `workflow`. New execution engines plug in by implementing `engine.Engine` against the `Host` capability interface, which keeps the runtime contract narrow. Memory services live in the separate `memory` module and depend on the SDK contracts rather than the other way around.

---

## Module map

| Module                                     | What it gives you                                                                                          | Stable        |
| ------------------------------------------ | ---------------------------------------------------------------------------------------------------------- | ------------- |
| [`sdk`](sdk/)                              | Core primitives — agent, graph DAG, kanban, model, llm, telemetry                                          | yes           |
| [`memory`](memory/)                        | Memory domain — recall v2, history, knowledge, retrieval, text, and memory adapters                        | yes           |
| [`sdkx`](sdkx/)                            | Provider implementations (OpenAI, Anthropic, DeepSeek, MiniMax, Volcengine) + non-memory extensions        | yes           |
| [`vessel`](vessel/)                        | In-process agent runtime — Captain, restart, probes, sidecars, per-run workspaces, assembly helpers        | yes           |
| [`cmd/vesseld`](cmd/vesseld/)              | Standalone daemon binary — declarative YAML, HTTP/SSE control plane                                        | yes           |
| [`voice`](voice/)                          | Real-time voice pipeline (VAD / STT / LLM / TTS / WebRTC)                                                  | yes           |
| [`examples/`](examples/)                   | Worked end-to-end examples (voice pipeline, multi-vessel daemon, …)                                        | —             |
| [`tests/quality/`](tests/quality/)         | Quality / regression suites (knowledge retrieval, vessel runtime)                                          | —             |
| [`tests/e2e/`](tests/e2e/)                 | Black-box end-to-end suites (vesseld subprocess)                                                           | —             |
| [`tests/conformance/`](tests/conformance/) | Provider conformance — same surface, every backend                                                         | —             |

---

## Highlights

### Hybrid memory that actually recalls (`memory/recall`)

- Three-lane retrieval (BM25 + vector + entity), fused via **Reciprocal Rank Fusion** (K=60), then re-weighted by entity-overlap boost, supersede decay, and time decay.
- Predicate alias normalisation so "favourite color" and "favorite colour" hit the same memory.
- Pluggable `retrieval.Index` backend — `memory/retrieval/memory` (in-memory), `memory/retrieval/sqlite` (SQLite), and `memory/retrieval/postgres` (Postgres + pgvector) ship in-tree; bring your own by implementing `retrieval.Index`.

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

- [pkg.go.dev/github.com/GizClaw/flowcraft/sdk](https://pkg.go.dev/github.com/GizClaw/flowcraft/sdk) — core primitives (agent, engine, graph, llm, tool, telemetry, …)
- [pkg.go.dev/github.com/GizClaw/flowcraft/memory](https://pkg.go.dev/github.com/GizClaw/flowcraft/memory) — recall, history, knowledge, retrieval, and text packages
- [pkg.go.dev/github.com/GizClaw/flowcraft/sdkx](https://pkg.go.dev/github.com/GizClaw/flowcraft/sdkx) — provider implementations
- [pkg.go.dev/github.com/GizClaw/flowcraft/vessel](https://pkg.go.dev/github.com/GizClaw/flowcraft/vessel) — runtime layer
- [pkg.go.dev/github.com/GizClaw/flowcraft/voice](https://pkg.go.dev/github.com/GizClaw/flowcraft/voice) — voice pipeline

Worked examples live under [`examples/`](examples/) — each one is runnable end-to-end with a single command.

For the daemon specifically, run `vesseld --help` for CLI sub-commands and supported YAML kinds; HTTP control-plane endpoints are documented inline on the command handlers.

---

## Status

`sdk` and `sdkx` are stable and released continuously. `memory` is published as its own module for recall, history, knowledge, retrieval, and text. `vessel` has shipped `v0.3.0` with per-run session workspaces and assembly helpers, and `cmd/vesseld` is production-ready for single-node deployments. Durable execution (Postgres + SQLite checkpoint stores), OTel exporters, Prometheus `/metrics`, the seven-suite `eval/` harness, and end-to-end `tests/e2e/vesseld` conformance are all in place.

The next milestone is the assertion-graph memory model: first-class observations, assertions, and links with provenance, so recall can retrieve linked evidence packets instead of isolated facts.

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

This repo is a Go workspace (`go.work`). The in-tree modules are `sdk`, `memory`, `sdkx`, `vessel`, `voice`, `cmd/vesseld`, and `eval`. Off-workspace examples and test harnesses pin released versions and run with `GOWORK=off`.

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
