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
  (agent and workspace settings).
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

- `deploy.yaml` — the `core/deploy` resource document: event bus, simulated
  tool source, tool assembly, workspace, DeepSeek provider, inference
  assembly, JS script runtime, and the graph agent.
- `inference.yaml` — provider profiles and secret resolvers (still used by
  forge's credential preflight).
- `workspace.yaml` / `tools.yaml` — authoring references kept alongside the
  new inline resource settings; the runtime consumes deploy.yaml directly.
- `graphs/assistant.yaml` — the graph definition, with `scripts/` and
  `prompts/` beside it (script sources and system prompts are referenced as
  `{"file": ...}`).
- `speakers.yaml` — optional user-facing labels per graph node; the TUI and
  test logs render each node's output under its label (e.g. `[主持人]`).

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
the forge directory at startup. Only `DEEPSEEK_API_KEY` is required: every
scenario routes to `deepseek-flash`. Two more providers are declared and
ready to use — `gpt-5.6-luna` through the OpenAI line-up (`OPENAI_API_KEY`)
and `glm-5.3-flash` through Zhipu's OpenAI-compatible endpoint
(`ZHIPU_API_KEY`) — and their references are lazy, so a missing key only
surfaces if a graph actually routes to them. Without any credential the app
fails with a clear message.

## TUI

`forge tui new` opens a two-panel TUI:

- **Chat** — send turns and watch streamed output.
- **Workspace** — workspace metadata and token usage.

`Tab` switches focus, `Enter` submits, `Esc` clears the focused input, and
`Ctrl+C` twice quits. Empty input is a no-op; type `/start` to open a fresh
story or `/next` to keep the story moving.

Two host commands configure inference without leaving the TUI:

- `/model` picks the backend — `auto` (the routing policy's default target) or
  any selectable `provider/name` the deployment exposes. `/model auto` and
  `/model <target>` skip the picker.
- `/think` picks the reasoning effort — `auto` (the graph's own default) or a
  canonical level (`minimal`/`low`/`medium`/`high`/`xhigh`). Providers fold
  these onto their own ladder and report any fold on the compile ledger.

Both choices live in the TUI process only: they are handed to the next turn as
engine inputs, and nothing is written to the workspace or the session. The
status line shows the current selection. Directives the scenario owns — such
as `/start` and `/next` — are not TUI commands and still reach the agent as
plain user text.

After each turn the Chat panel shows that turn's token accounting under the
input box: input / output / total tokens, reasoning
tokens, cache read / write tokens, and call count. Usage is mirrored from the
runtime host through `core/runtime`'s `WithHostFactory` decorator; the runtime
remains the owner of usage aggregation.

Chat output is split per speaker: every graph node's streamed text appears as
its own labelled block, and tool invocations appear as separate
`[工具调用]` / `[工具结果]` blocks instead of being narrated inline.

## How it wires into the stack

- `core/deploy` + `core/runtime` assemble the runtime from `deploy.yaml`;
  `core/runtime/session` drives turns and streams deltas to sinks.
- The graph engine is `core/graph/resource`, with script nodes running on the
  bundled JS runtime (`core/agent/scriptrt/jsrt`).
- Simulated tools are a `tool.Source` resource registered from
  `internal/simtools`; the DeepSeek provider is a `driver/openai` instance
  pointed at `https://api.deepseek.com` with a declared catalog.
- `WithHostFactory` wraps the session host so every LLM call's token usage is
  mirrored onto the app for TUI display.

## Development

```bash
go build ./...
go vet ./...
```
