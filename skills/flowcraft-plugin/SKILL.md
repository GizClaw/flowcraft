---
name: flowcraft-plugin
description: Author, load, and troubleshoot FlowCraft plugins (the backends/plugin module). Use when writing or reviewing plugin.yaml manifests, wiring the deployment document's plugins section (dirs + enabled whitelist), loading plugins with plugin.Loader, merging plugin layers into a deployment, authoring or debugging stdio/HTTP JSON-RPC service plugins, handling capability conflicts, version constraints, Reconcile diffs, or the inference.Provider/rpc adapter. Also use when extending the module itself.
---

# FlowCraft Plugin Authoring

Author plugin manifests, load them with the plugin shell, and wire
plugin capabilities into FlowCraft deployments.

## Quick start

1. **Write a manifest.** `plugin.yaml` is strictly decoded; unknown
   fields fail the load. Read [references/plugin.md](references/plugin.md#2-manifest-reference) before writing one.
2. **Declare the plugin in the deployment document.**

   ```yaml
   plugins:
     dirs: [./plugins]
     enabled: [acme.notion-tools@^1.0.0]
   ```

   An absent `enabled` list enables nothing. A name that matches no
   discovered plugin fails with NotFound; a version mismatch leaves the
   plugin disabled (the "waiting for upgrade" state) without error.
3. **Load and wire.** Parse the section with `plugin.ParsePluginsSection`,
   load with `plugin.NewLoader(...)`, apply into a fresh target, merge
   `set.Layers` into the document, then build with the runtime:

   ```go
   target := plugin.NewTarget()
   loader := plugin.NewLoader(
       plugin.WithCoreVersion("0.4.0"),
       plugin.WithServicePluginBuilder(remote.NewPlugin),
   )
   set, err := loader.Load(ctx, cfg)
   set.Apply(ctx, target)
   merged, _, err := deploy.LoadLayers(ctx, append(baseLayers, set.Layers...))
   runtime.NewBuilder(target.Resources).Build(ctx, merged)
   ```

## When to read references/plugin.md

- Writing or validating a manifest: read [§2 manifest reference](references/plugin.md#2-manifest-reference) and [§3 validation rules](references/plugin.md#3-validation-rules).
- Wiring or debugging a load: read [§4 loader usage](references/plugin.md#4-loader-usage) and [§5 enabled whitelist](references/plugin.md#5-enabled-whitelist-semantics).
- Authoring a service plugin or debugging RPC: read [§6 service slot protocol](references/plugin.md#6-service-slot-protocol) and [§7 remote adapter](references/plugin.md#7-inferenceproviderrpc-adapter).
- Before changing module behavior: read [§8 pitfalls](references/plugin.md#8-pitfalls) and run the module gates (`go test ./...`, `go vet ./...`, `golangci-lint run ./...`).

## Key facts

- A plugin is a directory with `plugin.yaml`; three slots: declaration
  layer (zero code), RPC service process (stdio/http JSON-RPC), native
  Go module (unchanged).
- Service plugins start lazily on first resource construction, not at
  apply time; the handshake result is authoritative for capabilities.
- stdio call timeouts tear the process down; never reuse a timed-out
  stdio stream.
- Plugin processes get a minimal environment (PATH + TMPDIR + declared
  `env`); host secrets are never inherited.
- `protocol_version` must be 0 (unset) or 1.
- Per-handle `resource.close` exists (`service.CloseHandle`), but the
  inference adapter's handle is only released on process teardown
  because `ProviderDefinition` is a value frozen by the runtime.

## Examples

- `backends/plugin/example/layer` — declaration-layer plugin.
- `backends/plugin/example/echo` — stdio JSON-RPC protocol reference.
- `backends/plugin/example/provider` — HTTP + SSE inference provider.
