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

To actually exercise the providers, populate a repo-root `.env` and
re-run `make conformance`. The loader walks up from each test's CWD,
so any of the candidates work.

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
