# Forge — Runnable Local Workspace Demo

Forge is the runnable local demo for the FlowCraft stack. It assembles a full
runtime from native deployment documents, opens an interactive TUI, runs
scripted scenario tests, and drives raid × persona simulations — all driven by
plain files under `scenarios/`, with no application code beyond the demo
itself.

中文版见 [README_zh.md](README_zh.md)。

## Quickstart

Prerequisites: Go 1.26+ and a provider credential (see
[Credentials](#credentials)).

```bash
cd examples/forge
go run . help
```

Run one scripted test:

```bash
go run . test -test werewolf/opening_setup
```

Create a workspace and inspect it:

```bash
go run . workspace create --config werewolf --workspace ./workspace
go run . workspace inspect --workspace ./workspace
```

## Commands

- `forge workspace create --config <raid> --workspace <dir>` — copy a raid
  scenario into a workspace.
- `forge workspace inspect --workspace <dir>` — print workspace metadata
  (agent, model, memory settings).
- `forge config raid|persona|test list` — list available scenarios and tests.
- `forge tui new` / `forge tui resume` — open the interactive TUI over a
  workspace.
- `forge test -test <raid>/<name> [--timeout 2m]` — run one scripted scenario
  test.
- `forge test-auto --raid <raid> --persona <persona> [--turns 3]` — simulate a
  dialogue between a persona agent and a raid agent.

## Scenarios

Scenario files are plain directories under `scenarios/`, resolved in priority
order from `--scenarios`, `FORGE_SCENARIOS`, the executable's directory, the
working directory, and the per-user config directory.

### Raids

Each `scenarios/raids/<name>/` is a complete workspace template:

- `deploy.yaml` — the `sdkx/deploy` document: resources (event bus, scheduler,
  workspace registry, inference assembly, tool assembly, JS script runtime),
  the graph agent, and runtime integrations.
- `inference.yaml` — provider profiles and secret resolvers.
- `workspace.yaml` — the workspace registry root and layout.
- `tools.yaml` — tool assembly policy; tool implementations are Go values
  registered from `internal/simtools`.
- `graphs/assistant.yaml` — the graph definition, with `scripts/` and
  `prompts/` beside it (script sources and system prompts are referenced as
  `{"file": ...}`).
- `speakers.yaml` — optional user-facing labels per graph node; the TUI and
  test logs render each node's output under its label (e.g. `[主持人]`).

A scenario may additionally deploy `memory.yaml` and wire the
`memory.context` / `memory.turn` hooks to enable long-term memory.

### Tests

`scenarios/tests/<raid>/<name>.yaml` defines one scripted test:

```yaml
name: werewolf_opening_setup
description: Starts a new Werewolf game and reveals the user as seat 3 villager.
raid: werewolf
turns:
  - 开始狼人杀
```

`forge test` copies the raid into `.out/<raid>_<timestamp>/`, runs every turn
through the session runtime, and writes `stats.txt` (per-turn metrics,
including failures) and `chat_log.txt`.

### Personas

`scenarios/personas/<name>/` are full workspace templates used by
`forge test-auto` as the second agent in the simulation.

## Credentials

Provider credentials are read from environment variables declared by the
`inference.yaml` secret resolvers (`resolver: env`). The demo loads `.env` from
the forge directory at startup; it currently carries `DEEPSEEK_API_KEY`, and
every scenario pins `deepseek-v4-flash`. Without a credential the app fails
with a clear message.

## TUI

`forge tui new` opens a three-panel TUI:

- **Recall** — memory queries (only meaningful when the workspace deploys
  memory).
- **Chat** — send turns and watch streamed output.
- **Workspace** — workspace metadata and token usage.

`Tab` switches focus, `Enter` submits, `Esc` clears the focused input, and
`Ctrl+C` twice quits. Empty input is a no-op; type `/start` to open a fresh
story or `/next` to keep the story moving. After each turn the Chat panel
shows that turn's token
accounting under the input box: input / output / total tokens, reasoning
tokens, cache read / write tokens, and call count. Usage is mirrored from the
runtime host through `sdkx/runtime`'s `WithHostFactory` decorator; the runtime
remains the owner of usage aggregation.

Chat output is split per speaker: every graph node's streamed text appears as
its own labelled block, and tool invocations appear as separate
`[工具调用]` / `[工具结果]` blocks instead of being narrated inline.

## How it wires into the stack

- `sdkx/deploy` + `sdkx/runtime` assemble the runtime from `deploy.yaml`;
  `sdkx/runtime/session` drives turns and streams deltas to sinks.
- The graph engine is `sdk/graph`, with script nodes running on the bundled JS
  runtime (`sdkx/agent/jsrt`).
- Memory hooks and the `flowcraft` memory implementation are registered, but a
  scenario only enables memory by deploying `memory.yaml` and declaring the
  hooks on its agent.
- Simulated tools are Go values registered from `internal/simtools`.
- `WithHostFactory` wraps the session host so every LLM call's token usage is
  mirrored onto the app for TUI display.

## Development

```bash
go build ./...
go vet ./...
```
