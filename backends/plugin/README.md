# FlowCraft Plugin Shell (`backends/plugin`)

The plugin system gives FlowCraft deployment documents new `(Kind, Impl)`
implementations for host-defined resource contracts. A plugin is a
directory with a strictly decoded `plugin.yaml`; the shell discovers,
validates, assembles, and reconciles plugin sets without touching the
host's resource/deploy/runtime contracts.

Three slots:

| Slot | Artifact | Delivery | Example |
| --- | --- | --- | --- |
| Declaration layer | `layer` | a `deploy.Layer` fragment merged into the deployment document, zero code | `example/layer` |
| Service | `service` | an external process speaking JSON-RPC over stdio or http | `example/echo`, `example/provider` |
| Native | — | compiled Go module registration (unchanged) | existing `Register` |

**Status**: pre-release. The module is an independent workspace module by
design; the plan is to fold it into `core` (`core/plugin`, `core/service`,
`core/plugin/remote`) once mature. The release gate currently plans only
`core`, so no `.release/` changeset should be added for this module until
the fold-in path is decided.

## Packages

- `plugin` — the shell: manifest parsing and validation, `Loader` with
  dependency/conflict checks and `Reconcile`, layer assembly, the
  deployment document `plugins` section decoder.
- `plugin/service` — the transport channel: process supervision
  (stdio/http), JSON-RPC 2.0 client, capability handshake, lazy start,
  timeouts, payload caps, per-handle `resource.close`.
- `plugin/remote` — contract adapters. v1 anchor: `inference.Provider`
  over RPC (unary `generate` + http/SSE `generate_stream`).
- `example/` — reference plugins.

## Quick start

### 1. Write a manifest

```yaml
# plugins/acme.notion-tools/plugin.yaml
name: acme.notion-tools          # lowercase reverse-domain, unique
version: 1.2.0                   # full major.minor.patch semver
requires:
  core: ">=0.4.0"
artifacts:
  - type: layer                  # declaration slot: zero code
    path: layers/10-notion.yaml
    priority: 100
  - type: service                # service slot: external RPC process
    transport: stdio             # or http
    command: python
    args: ["-m", "acme_plugin", "--stdio"]
    env:
      NOTION_TOKEN: ${env:NOTION_TOKEN}
    protocol_version: 1
    capabilities:
      - kind: inference.Provider
        impl: acme.notion
```

`plugin.yaml` is strictly decoded: unknown fields, bad names/versions,
unsupported artifact types, duplicate capabilities, and
`protocol_version` other than 1 are load-time `Validation` errors.

### 2. Declare the plugin in the deployment document

```yaml
plugins:
  dirs:
    - ./plugins
  enabled:                      # optional; an absent whitelist enables nothing
    - acme.notion-tools@^1.0.0
```

An entry naming no discovered plugin fails with `NotFound`; an entry whose
version constraint is unsatisfied leaves the plugin disabled without error
(the "waiting for upgrade" state).

### 3. Load and wire

```go
target := plugin.NewTarget() // fresh resource.Registry
loader := plugin.NewLoader(
    plugin.WithCoreVersion("0.4.0"),                    // satisfies requires.core
    plugin.WithServicePluginBuilder(remote.NewPlugin),  // service artifacts -> RPC plugins
)
set, err := loader.Load(ctx, cfg) // cfg from plugin.ParsePluginsSection
set.Apply(ctx, target)            // register factories; defer set.Close()

// Merge plugin layers into the deployment document and build normally:
merged, _, err := deploy.LoadLayers(ctx, append(baseLayers, set.Layers...))
runtime.NewBuilder(target.Resources).Build(ctx, merged)
```

`loader.Reconcile(ctx)` re-runs the same directories and config and returns
`(*Set, Changes, error)`; on failure the previous projection is retained.
Version transitions drive the diff (satisfying a constraint → `Added`,
falling out → `Removed`), manifest/layer changes → `Changed`.

## Manifest reference

| Field | Type | Rules |
| --- | --- | --- |
| `name` | string | Required, lowercase reverse-domain, labels ≤ 63 chars |
| `version` | string | Required, full `major.minor.patch` semver (`v` prefix optional) |
| `description` | string | Optional |
| `requires.core` | string | Optional semver range; checked when the loader has a core version |
| `requires.plugins` | []string | Optional `name@constraint` list; cycle-checked |
| `provides` | []resource.Spec | Optional capability declarations, deduplicated |
| `artifacts` | []artifact | `type: layer` or `service` (`wasm` reserved, rejected) |

Layer artifact: `path` (required, must stay inside the plugin directory),
`priority` (aligns with `deploy.Layer.Priority`).

Service artifact: `transport` (`stdio` requires `command`; `http` requires
`url`), `args`, `env` (minimal injection — see below), `headers` (http),
`protocol_version` (0 or 1), `capabilities` (deduplicated across artifacts;
may mirror `provides`).

## Service slot protocol

JSON-RPC 2.0, newline-delimited on stdio or POSTed to a fixed URL on http;
the host is always the client. Methods: `plugin.handshake`,
`resource.new`, `resource.call`, `resource.close`.

- **Handshake**: the host sends its supported `protocol_versions`
  (currently `[1]`) and core version; the plugin replies with the highest
  common version and its authoritative capabilities. A version mismatch
  fails with `NotAvailable`.
- **Lazy start**: the process spawns on the first `resource.new`, with
  3 retries and backoff on startup failure.
- **Timeouts and limits**: per-call default 30s (spec override), payload
  cap 8 MiB (spec override). An http timeout leaves the service healthy;
  a **stdio timeout tears the process down** and the next use starts fresh.
- **Environment**: the plugin process gets `PATH` + `TMPDIR` + the declared
  `env` only; host secrets are never inherited.
- **Lifecycle**: `Service.New` / `Call` / `CloseHandle` per resource;
  `Service.Close` SIGTERM → 5s grace → SIGKILL. Per-handle `resource.close`
  exists via `CloseHandle`.
- **Errors**: RPC errors and request-level rejections leave the service
  healthy; transport failures map to `NotAvailable` and the host recreates
  the service.

## inference.Provider/rpc adapter

`remote.NewPlugin` accepts only `inference.Provider` capabilities (the v1
anchor). Settings strictly decode `id` + `model`/`models`. The factory
starts the service, verifies the handshake-declared capability before
constructing a handle, binds unary `generate` (plus SSE `generate_stream`
when declared and the transport is http), and returns a
`ProviderDefinition`.

Note: `ProviderDefinition` is a value frozen by the inference runtime, so
deploy's reverse-close cannot reach the RPC handle; `rpcProvider`
implements `io.Closer` for direct users, and process teardown remains the
guaranteed release for runtime-managed providers.

## Examples

- [`example/layer`](example/layer/) — a declaration-layer plugin (zero
  code): `plugin.yaml` + one layer fragment + a shipped loader test.
  Verify with `go test ./example/layer`.
- [`example/echo`](example/echo/) — a minimal stdio plugin implementing
  the JSON-RPC protocol; the Go-side reference for the service slot.
- [`example/provider`](example/provider/) — an http + SSE
  `inference.Provider` plugin demonstrating the v1 anchor end to end.

The `remote` package's e2e tests drive a provider through handshake,
resource construction, unary and streaming generation.

## Verification

```bash
cd backends/plugin
go test ./... -race -count=1
go vet ./...
golangci-lint run ./...
go mod tidy -diff
```

The module is picked up automatically by the repo `Makefile`
(`make ci`, `make lint`) through `backends/*/go.mod`.

## Plugin authoring skill

[`skills/flowcraft-plugin`](../../skills/flowcraft-plugin/) is a Codex skill
for authoring, loading, and troubleshooting FlowCraft plugins: the
`plugin.yaml` manifest, the deployment document's `plugins` section, loader
wiring, the service slot protocol, and the `inference.Provider`/rpc
adapter. Install it into Codex:

```bash
python3 ~/.codex/skills/.system/skill-installer/scripts/install-skill-from-github.py \
  --repo GizClaw/flowcraft --path skills/flowcraft-plugin --ref main
```

## Roadmap

- CLI (`flowcraft plugin list/validate`) — not started.
- Runnable end-to-end wiring in a host (`examples/forge`) — not started;
  the loader is exercised only by tests and examples so far.
- Fold into `core` (`core/plugin`, `core/service`, `core/plugin/remote`)
  and the accompanying release decision.
