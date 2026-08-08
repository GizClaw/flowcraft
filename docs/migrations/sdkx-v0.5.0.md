# Migrating to sdkx v0.5.0

> Status: `sdkx/v0.5.0`, coordinated with `sdk/v0.5.0`. This release pins
> `sdk v0.5.0` and rebuilds the provider / adapter layer accumulated through
> the `v0.4.x` line: the legacy inference stacks are unified under
> `sdkx/inference`, the sandbox backend moves from nsjail to bubblewrap, the
> assembly surface (`deploy`, `runtime`, `scheduler`, `delegation`, `memory`
> glue) becomes the supported way to wire applications, and the deprecated
> `0.4.x` adapters are removed in one cut.

## Summary

`sdkx/v0.5.0` is the provider / adapter half of the "unified runtime"
cutover documented in [`v0.5.0.md`](v0.5.0.md). Four themes drive it:

1. **Unified inference providers** — `sdkx/llm/<provider>` and
   `sdkx/embedding/<provider>` are gone. Providers implement the
   `sdk/inference` compiler contract in `sdkx/inference/<provider>`, with
   hardened protocol options, provider request/response ID propagation to
   telemetry, and the `sdk/inference` router's retry / backoff / circuit
   breaker.
2. **Sandbox cutover** — `sdkx/sandbox/nsjail` is replaced by
   `sdkx/sandbox/bwrap` (Linux, namespace-level FS/net with network policy)
   and `sdkx/sandbox/seatbelt` (macOS).
3. **Assembly surface** — `sdkx/deploy` (one YAML document → runnable
   `Instance`s), `sdkx/runtime` (+ `sdkx/runtime/session`), `sdkx/scheduler`,
   `sdkx/delegation` (+ `kanban` backend), and `sdkx/memory` (`hook`,
   `render`) become the supported composition layer built on the unified
   `sdk/config` `Factory` / `Source` / `Loader` protocol.
4. **Legacy removal** — `sdkx/engine`, `sdkx/claw`, `sdkx/extract`,
   `sdkx/recall`, `sdkx/retrieval`, and `sdkx/tool/{history,knowledge,kanban,
   memory}` are deleted; the module drops its `memory` module dependency.

---

## Module surface changes (vs `sdkx/v0.4.x`)

### Removed

| Removed (`sdkx`)                                   | Replacement                                                                              |
| -------------------------------------------------- | ---------------------------------------------------------------------------------------- |
| `sdkx/llm/<provider>`, `sdkx/embedding/<provider>` | `sdkx/inference/<provider>` — providers implement the `sdk/inference` compiler contract |
| `sdkx/engine`, `sdkx/claw`, `sdkx/extract`         | removed with the legacy runtimes                                                          |
| `sdkx/recall`, `sdkx/retrieval`                    | the `memory` module's own stores and adapters                                             |
| `sdkx/tool/{history,knowledge,kanban,memory}`      | `sdkx/delegation/kanban`, `sdkx/tool/{mcp,delegation}`, application-owned tools          |
| `sdkx/sandbox/nsjail`                              | `sdkx/sandbox/bwrap` (Linux) + `sdkx/sandbox/seatbelt` (macOS)                            |

The `memory` module dependency carried by `sdkx/v0.4.x` for the deprecated
retrieval wrappers is removed together with those wrappers; `sdkx/v0.5.0`
depends only on `sdk v0.5.0`.

### New

- `sdkx/inference/<provider>` — unified provider adapters (anthropic, azure,
  bytedance, deepseek, kimi, minimax, openai, qwen) plus protocol-level HTTP
  options, provider request/response IDs in telemetry, and router
  retry / backoff / circuit breaker integration.
- `sdkx/agent/a2a` (+ `sdkx/agent/a2a/config`, engine kind `a2a`) — A2A
  remote-proxy engine covering protocol 0.3 and 1.0 over JSON-RPC and gRPC,
  with bearer/basic/custom auth and stream / poll modes.
- `sdkx/agent/{jsrt,luart}` — script runtimes moved from the retired
  `sdk/script` domain; register as `agent.ScriptRuntime` (`js` / `lua`).
- `sdkx/deploy` — declarative assembly: one YAML document names shared
  resources, agents, engines, and lifecycle hooks; one `Build` call produces
  runnable `Instance`s. Document references use the unified `config.Source` /
  `config.Loader` forms (`{file:}`, `{embed:}`) and are capped at 1 MiB.
- `sdkx/runtime` (+ `sdkx/runtime/session`) — transport-neutral application
  core above a `deploy.Result`: event router, integrations, session manager,
  interruptible streaming turns.
- `sdkx/delegation` (+ `sdkx/delegation/kanban` and its `config` resource) —
  async delegation backend implementing the `sdk/delegation` contracts.
- `sdkx/scheduler` — in-process cron / timer / execution-queue / lease /
  memory and delegation adapters for `sdk/scheduler`.
- `sdkx/memory` (`hook`, `render`) — generic memory glue on the `sdk/memory`
  contracts (GoTemplate renderer, context/turn lifecycle hooks).
- `sdkx/tool/mcp` — expanded MCP bridge (`adapter`, `config`, `source`,
  `transport`, `errors`); `sdkx/tool/delegation` — `delegate` /
  `delegation_status` tools.
- `sdkx/workspace/objstore/s3` — S3 object-store driver registration updated
  for the tightened storage contract.

### Included from the `v0.4.x` line

The `0.4.x` line accumulated provider work that this release rebuilds and
carries forward, most of which now lives in `sdkx/inference/<provider>`:

- OpenAI Responses API adapters and stream-error handling.
- MiniMax OpenAI-compatible provider, ByteDance Responses API cutover, and
  Anthropic thinking-option wiring.
- Provider HTTP-client hardening (protocol options) and request/response ID
  telemetry.

## Upgrade path

1. **Bump modules** — `sdk` first, then `sdkx` (pins `sdk v0.5.0`):

   ```bash
   go get github.com/GizClaw/flowcraft/sdk@v0.5.0
   go get github.com/GizClaw/flowcraft/sdkx@v0.5.0
   go mod tidy
   ```

2. **Provider imports** — `sdkx/llm/*` and `sdkx/embedding/*` →
   `sdkx/inference/*`; providers now implement the `sdk/inference` compiler
   contract instead of the removed `sdk/llm` / `sdk/embedding` surfaces.
3. **Sandbox** — switch `nsjail` configurations to `sdkx/sandbox/bwrap`
   (Linux) or `sdkx/sandbox/seatbelt` (macOS) and honor the network policy
   (non-default net modes need a namespace/container backend).
4. **Memory adapters** — stop importing `sdkx/recall`, `sdkx/retrieval`, and
   `sdkx/tool/{history,knowledge,kanban,memory}`; depend on the `memory`
   module for storage and on `sdkx/delegation/kanban` +
   `sdkx/tool/{mcp,delegation}` for the tool surface.
5. **Assembly** — if you adopt `sdkx/deploy`, use the new `config.Source`
   forms (`{file:}`, `{embed:}`), keep documents under 1 MiB, and register
   every factory / engine kind before `Build`; `sdkx/runtime` owns the
   deployment result and process lifecycle.

## Backwards compatibility

- No global shim. All `v0.4.x`-era deprecation surfaces listed above are
  removed in this release.
- `sdkx` no longer depends on the `memory` module; applications that need
  memory storage depend on `memory` directly.
- The `sdk/v0.5.0` migration guide remains the reference for the SDK-side
  cutover (`sdk/engine`, `sdk/llm`, `sdk/embedding`, `sdk/model`,
  `sdk/script` → `sdk/agent` + `sdk/inference` + `sdk/message` +
  `sdk/config`).

## Timeline

- **`sdkx/v0.4.x`** (released): deprecation window — legacy adapters stayed
  importable while the `sdk` and `memory` modules prepared the split.
- **`sdk/v0.5.0`** (released 2026-08-08): SDK-side unified-runtime cutover.
- **`sdkx/v0.5.0`** (this release): provider / adapter rebuild pinned to
  `sdk v0.5.0`; all breaking changes above land as one cut.
- **`memory`** (pending): awaits its own coordinated pin bump to
  `sdk v0.5.0`; see the memory module's release notes.
