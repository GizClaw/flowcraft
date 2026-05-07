# conformance

Manual conformance suites that exercise sdkx providers against their
real HTTP endpoints. They live outside `sdk/` and `sdkx/` because:

- They require live API keys, so they cannot run in CI by default.
- They are developer tools, not part of the SDK's public surface.
- Keeping them in a separate module isolates their dependency graph
  from sdk/sdkx releases.

## Running

All suites self-skip when the relevant env vars are missing, so a
credential-less run is a no-op (useful as a compile check):

```bash
make conformance
```

To actually exercise the providers, copy `.env.example` from the repo
root to `.env`, fill in credentials, and re-run `make conformance`.
The loader walks up from each test's CWD, so any of the candidates
work. `.env` is git-ignored; `.env.example` is the source-of-truth
template.

## Required env vars

### LLM providers (`tests/conformance/llm/`)

Each provider is configured via a single JSON env var. Tests for an
unset provider are skipped.

| Env var | Provider |
| --- | --- |
| `FLOWCRAFT_TEST_MINIMAX` | minimax |
| `FLOWCRAFT_TEST_QWEN` | qwen |
| `FLOWCRAFT_TEST_BYTEDANCE` | bytedance |
| `FLOWCRAFT_TEST_AZURE` | azure |
| `FLOWCRAFT_TEST_DEEPSEEK` | deepseek |

### LLM image-generation providers (`tests/conformance/llm/image_test.go`)

Same JSON shape as the chat providers, registered under separate
provider keys so chat and image catalogs stay decoupled. See
`.env.example` at the repo root for ready-to-paste templates.

| Env var | Provider key | Default model |
| --- | --- | --- |
| `FLOWCRAFT_TEST_MINIMAX_IMAGE` | `minimax-image` | `image-01` |
| `FLOWCRAFT_TEST_BYTEDANCE_IMAGE` | `bytedance-image` | `doubao-seedream-5-0-260128` |
| `FLOWCRAFT_TEST_QWEN_IMAGE` | `qwen-image` | `qwen-image-2.0-pro` |

Scenarios:

- Basic text-to-image (all three).
- Explicit `Width`/`Height` mapping (verifies `WxH` vs `W*H` size syntax).
- Streaming via `NewOneChunkStream` (single chunk + final message).
- Image-to-image with a reference URL (`minimax-image`, `bytedance-image`).
- `qwen-image` rejection of `PartImage` inputs (the t2i-only contract).

Generated image URLs are normally just logged. To download every
returned image to `tests/conformance/llm/_out/` for offline visual
review (the directory is git-ignored), set `SAVE_GENERATED_IMAGES=1`:

```bash
SAVE_GENERATED_IMAGES=1 make test-conformance
```

JSON shape:

```json
{
  "provider": "azure",
  "api_key": "...",
  "model": "gpt-5",
  "base_url": "...",
  "caps": {"no_temperature": true}
}
```

`caps` is a test-fixture convention only — `llm.NewFromConfig` ignores
it; the suite reads it to decide whether to skip scenarios that need
a capability the model doesn't support.

### Embedding providers (`tests/conformance/embedding/`)

| Env var | Notes |
| --- | --- |
| `EMBEDDING_PROVIDER` | e.g. `azure`, `openai`, `qwen`, `bytedance` |
| `EMBEDDING_API_KEY` | required |
| `EMBEDDING_MODEL` | e.g. `text-embedding-3-large` |
| `EMBEDDING_BASE_URL` | required for azure |
| `EMBEDDING_API_VERSION` | optional, azure |

## Adding a new suite

Drop a new directory under `tests/conformance/` with:

- `doc.go` declaring the package and documenting the env vars.
- One or more `_test.go` files that `t.Skip` cleanly when their env
  is missing.
- Import `github.com/GizClaw/flowcraft/tests/conformance/internal/testenv`
  and call `testenv.Load()` from `init()` if you need `.env` loading.
