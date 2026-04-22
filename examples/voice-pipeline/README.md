# Voice pipeline example

Local demo: **microphone → ByteDance STT → FlowCraft graph (MiniMax LLM) → MiniMax TTS → speakers**, with optional **text input** and **barge-in**. It is intentionally minimal (no persona or extra app logic)—only the voice stack and a small terminal UI.

## Requirements

- **Go** 1.25+
- **macOS** (default build uses PortAudio for mic and speakers)
- **PortAudio**: `brew install portaudio`
- **Accounts / keys**
  - ByteDance (Volcengine) speech: app id + access token for STT
  - MiniMax: API key for LLM (Anthropic-compatible endpoint) and TTS

## Configuration

1. Copy `deploy/.env.example` to `deploy/.env` or a repo-root `.env`, and set at least:

   | Variable | Purpose |
   |----------|---------|
   | `FLOWCRAFT_VOICE_BYTEDANCE_APP_ID` | ByteDance STT app id |
   | `FLOWCRAFT_VOICE_BYTEDANCE_ACCESS_TOKEN` | ByteDance STT token |
   | `FLOWCRAFT_VOICE_MINIMAX_API_KEY` | MiniMax API key |

   Legacy names (`BYTEDANCE_*`, `ANIMUS_*`, etc.) and `FLOWCRAFT_TEST_MINIMAX` JSON are also supported; see `env.go` and `deploy/.env.example`.

2. Optional:

   - `FLOWCRAFT_VOICE_MINIMAX_MODEL` — default model ref for the LLM resolver fallback (short name or `minimax/...`).
   - `FLOWCRAFT_VOICE_MINIMAX_VOICE_ID` — TTS voice (default in code: `male-qn-qingse`).

3. **Graph**: the program loads **`react_agent.yaml`** from the **current working directory** (same format as FlowCraft server YAML import: top-level `name`, `entry`, `nodes`, `edges`). Edit the `llm` node `config` for `system_prompt`, `temperature`, `model`, etc.

## Run

From **`examples/voice-pipeline`** (so `react_agent.yaml` is found):

```bash
cd examples/voice-pipeline
go run .
```

If you use `../../deploy/.env`, run from this directory so `loadDotEnv()` can merge those files.

## What the UI does

- Shows partial/final transcripts, assistant streaming text, and simple turn metrics.
- Text input instead of speaking; **`/voice`** lists or sets TTS voice when the API returns a list.
- **`/reset`**: stops the current generation/playback (`Pipeline.Abort` + `Session.StopSpeaking`) and clears the on-screen log. This example does **not** enable `workflow.WithMemoryFactory`, so there is no persisted multi-turn server memory to wipe—only in-flight work and the terminal buffer.

## Project layout (short)

| File | Role |
|------|------|
| `main.go` | Entry: env check, PortAudio, delegates to setup + UI |
| `setup.go` | Workflow runtime, cloud STT/TTS, `speech.Pipeline` options |
| `run_ui.go` | Mic session + bubbletea program; `/reset` wiring |
| `bridge_tui.go` | Maps speech metrics/events into bubbletea messages |
| `react_agent.yaml` | Declarative graph definition |
| `graph_load.go` | Reads and validates `react_agent.yaml` |
| `provider_store.go` | In-memory `ProviderConfigStore` for MiniMax credentials |
| `env.go` | Environment helpers and dotenv merge |
| `portaudio.go` | Mic / speaker I/O |
| `tui.go` | bubbletea chat UI |

## Troubleshooting

- **Missing keys**: the binary exits early with a hint to set the `FLOWCRAFT_VOICE_*` variables.
- **Wrong directory**: run from `examples/voice-pipeline` so `react_agent.yaml` resolves.
- **No audio**: check macOS microphone permission and that PortAudio sees your default input/output devices.
