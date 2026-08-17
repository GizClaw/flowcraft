# FlowCraft Plugin Reference

## Table of contents

1. [Concepts](#1-concepts)
2. [Manifest reference](#2-manifest-reference)
3. [Validation rules](#3-validation-rules)
4. [Loader usage](#4-loader-usage)
5. [Enabled whitelist semantics](#5-enabled-whitelist-semantics)
6. [Service slot protocol](#6-service-slot-protocol)
7. [inference.Provider/rpc adapter](#7-inferenceproviderrpc-adapter)
8. [Pitfalls](#8-pitfalls)

## 1. Concepts

The plugin shell (`backends/plugin`) gives deployment documents new
(Kind, Impl) implementations for host-defined resource contracts. Three
slots:

| Slot | Artifact type | Delivery | Example |
| --- | --- | --- | --- |
| Declaration layer | `layer` | a `deploy.Layer` fragment, zero code | `example/layer` |
| Service | `service` | external process + JSON-RPC (stdio/http) | `example/provider` |
| Native | — | compiled Go module registration (unchanged) | existing `Register` |

The shell depends only on core protocol packages (`resource`, `deploy`,
`errdefs`) and knows no concrete resource kinds; the RPC adapter for a
kind lives in `backends/plugin/remote`. The module is not yet part of
`core`; the plan is to fold it in (`core/plugin`, `core/service`,
`core/plugin/remote`) once mature.

## 2. Manifest reference

`plugin.yaml` is strictly decoded (unknown fields are errors). Fields:

| Field | Type | Rules |
| --- | --- | --- |
| `name` | string | Required, lowercase reverse-domain (e.g. `acme.notion-tools`), labels ≤ 63 chars |
| `version` | string | Required, full `major.minor.patch` semver (`v` prefix optional) |
| `description` | string | Optional |
| `requires.core` | string | Optional semver range; checked only when the loader has a core version |
| `requires.plugins` | []string | Optional `name@constraint` list; cycle-checked |
| `provides` | []resource.Spec | Optional capability declarations, deduplicated |
| `artifacts` | []artifact | Optional; `type` is `layer` or `service` (`wasm` reserved, rejected) |

Layer artifact: `path` (required, must resolve inside the plugin dir),
`priority` (aligns with `deploy.Layer.Priority`).

Service artifact: `transport` (`stdio` requires `command`; `http`
requires `url`), `args`, `env` (minimal injection, see §6), `headers`
(http), `protocol_version` (0 or 1), `capabilities` (declared
capability specs, deduplicated across artifacts; may mirror `provides`).

## 3. Validation rules

- Name/version/constraint syntax errors → `errdefs.Validation` with the
  plugin name and version.
- Duplicate `provides` or duplicate service capabilities → Validation.
- Unsupported artifact type or `protocol_version` → Validation.
- Layer path traversal (`..`, absolute paths), missing files, or
  non-decodable layer content → Validation.
- `(kind, impl)` colliding with an already registered factory or with
  another plugin → `errdefs.Conflict`.
- Missing plugin dependency → `errdefs.NotFound`; dependency cycle →
  Validation.

## 4. Loader usage

```go
cfg, err := plugin.ParsePluginsSection(docBytes) // strict decode
loader := plugin.NewLoader(
    plugin.WithCoreVersion("0.4.0"),                       // satisfies requires.core
    plugin.WithTarget(target),                            // conflict detection registry
    plugin.WithServicePluginBuilder(remote.NewPlugin),    // service artifacts -> plugins
)
set, err := loader.Load(ctx, cfg)  // or Load(ctx, cfg, extraDirs...)
set.Apply(ctx, target)             // register factories
defer set.Close()                  // reverse-order Close
```

`Loader` is instance-scoped, holds no global state, and is safe for
concurrent use. `Reconcile(ctx)` re-runs the same dirs/config and
returns `(*Set, Changes, error)`; on failure the previous projection is
retained. Load order is by plugin name; layers are sorted by ascending
priority. An absent `enabled` list returns an empty set without
scanning directories.

## 5. Enabled whitelist semantics

- `plugins.enabled` absent → nothing enabled (explicit-enable default).
- An entry naming no discovered plugin → `NotFound` (typo protection;
  also applies during Reconcile).
- An entry whose version constraint is unsatisfied → plugin stays
  disabled, no error. Version transitions drive Reconcile diffs:
  satisfying a constraint produces `Added`, falling out produces
  `Removed`; manifest/layer content changes produce `Changed`.
- Removing an enabled plugin's directory → Reconcile fails with
  NotFound and keeps the previous projection (a broken deployment, not
  a silent removal).

## 6. Service slot protocol

JSON-RPC 2.0, newline-delimited on stdio or POSTed to a fixed URL on
http. Host is always the client. Methods: `plugin.handshake`,
`resource.new`, `resource.call`, `resource.close`.

- **Handshake**: host sends `protocol_versions` (currently `[1]`) and
  host core version; plugin replies with the highest common version and
  its authoritative `capabilities`. Missing common version → NotAvailable.
- **Lazy start**: process spawns on the first `resource.new`; start
  failure retries 3× with backoff, then NotAvailable.
- **Timeouts**: per-call default 30s (spec override); payload cap 8 MiB
  (spec override). HTTP per-call timeout leaves the service healthy; a
  **stdio timeout tears the process down** and the next use starts
  fresh (the abandoned reader would otherwise poison the stream).
- **Environment**: `PATH` + `TMPDIR` + declared `env` only. Host
  secrets are never inherited.
- **Lifecycle**: `Service.New/Call/CloseHandle` per resource;
  `Service.Close` SIGTERM → grace (5s) → SIGKILL; process teardown also
  releases every handle. `Healthy()` is false after any transport
  failure; the host must recreate the service.
- **Errors**: plugin RPC errors and request-level rejections
  (Validation/NotFound) leave the service healthy; transport failures
  map to NotAvailable.

## 7. inference.Provider/rpc adapter

`remote.NewPlugin` accepts only `inference.Provider` capabilities (v1
anchor). `providerFactory.New`:

1. Strictly decodes settings (`id` + `model`/`models` required);
2. Starts the service and checks the handshake-declared capability
   before constructing a handle;
3. Calls `resource.new`, binds unary `generate` (and SSE
   `generate_stream` when the capability declares streaming and the
   transport is http), and returns a `ProviderDefinition`.

Streaming uses the service's pooled client without a total timeout;
callers cancel the context to end the stream. Because
`ProviderDefinition` is a value frozen by the inference runtime, the
deploy builder's reverse-close cannot reach the RPC handle —
`rpcProvider` implements `io.Closer` for direct users, and process
teardown remains the guaranteed release for runtime-managed providers.

## 8. Pitfalls

- **Enabled typo**: a misspelled `enabled` name fails with NotFound —
  do not silence it; the whole plugin set would silently be empty.
- **stdio reuse after timeout**: never call a stdio service again after
  a per-call timeout; it is torn down and must be recreated.
- **Env inheritance**: plugin processes do not inherit host secrets;
  declare credentials explicitly in the service artifact `env`.
- **Strict decode**: unknown manifest fields, unknown artifact types,
  and `protocol_version != 1` are load-time errors, not warnings.
- **Path safety**: layer and service file paths must stay inside the
  plugin directory; the resource loader rejects `..` and absolute paths.
- **ProviderDefinition freezing**: per-handle close is not reached via
  deploy rollback for the inference adapter; rely on process teardown
  or explicit `rpcProvider.Close()`.
- **Reconcile vs config changes**: dirs, whitelist, and loader options
  are fixed at Load; change them with a fresh Load, not Reconcile.
- **Release status**: the module is not yet in `core` and the release
  gate (`tools/releasegate`) only plans `core`; do not add a
  `.release/` changeset for it without first deciding the fold-in path.
