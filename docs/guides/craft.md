---
layout: default
title: Craft Applications
---
# Craft Applications

`craft` assembles and owns a FlowCraft application: the `craft.yaml`
definition, the shared services of the running app, and a keyed set of
runtimes built from deployment documents. It sits between a shell
(desktop, HTTP, headless) and `core/runtime`. The shell owns the process
and the presentation; craft owns the application.

## What a Craft is

- A **definition** — one `craft.yaml`: application metadata, optional UI
  delivery, plugin roots and mount points, `host_tools` configuration, and
  either an inline `deploy` document or a list of `base_layers`.
- **Capabilities** — compile-time Go extensions passed to `craft.New` as
  `[]craft.Capability`. They register resource factories, service
  implementations, layers, `${...}` resolvers and host factory decorators.
- The **running Craft** — shared services (configuration resolution, the
  craft event plane, the plugin host, the host primitive server) plus a
  map of `core/runtime.Runtime` values keyed by `craft.RuntimeKey`.

A runtime is built from the composed deployment document exactly as
`core/deploy` and `core/runtime` would build it; craft assembles the
document, injects externals, and binds services afterwards.

**What craft is deliberately not:**

- Not a webview, desktop shell or renderer. It delivers UI assets and a
  `ui` declaration; loading and drawing belong to the shell.
- Not a frontend component model.
- Not a plugin sandbox. A plugin child process runs with the calling
  user's privileges; the permission model is an API grant, not isolation.
- Not a workspace manager. Craft offers a keyed runtime set; the meaning
  of a key (a workspace, a profile, a tenant) and any pooling policy are
  the application's.

## The two extension axes

| | Capability | Plugin |
| --- | --- | --- |
| Carrier | Go code | `plugin.json` + data + optional UI bundle + optional MCP server |
| When | compile time (`craft.New`) | install / enable / disable |
| Registers resource kinds | yes | **no** |
| Typical contribution | domain resources, primitive service implementations, layers, resolvers, decorators | tools (MCP), graph nodes, skills, hooks, UI |
| Permission model | none (trusted code) | manifest grants checked by the host |
| Lifetime | the process | the plugin's enable state |

The third input is YAML, which composes and configures but never defines
behaviour: a definition can only select and configure a resource kind an
already-registered factory provides, a craft built-in, or a service
implementation. In particular, **a plugin can never register a Go
resource kind**. The registry is frozen by `craft.New`; plugin changes
touch only the deployment document and the shared tool set, so
hot-plugging never tears the resource registry.

## Definition schema

```yaml
craft:
  id: hello                 # required
  name: Hello               # optional
  version: 0.1.0            # required
  min_host_version: 0.1.0   # optional compatibility floor

ui:                         # host app frontend, not the plugin channel
  assets: dist              # optional
  entry: dist/index.html    # optional
  kinds: [panel, command]   # optional

plugins:                    # optional
  builtin: builtin-plugins  # read-only plugin directory
  roots: [plugins]          # directories the host scans
  tool_registry: tools      # resource that receives plugin tools
  node_targets: [assistant] # agents that receive plugin node types

host_tools:                 # optional
  disable: [open_url]       # primitives never exposed
  services:                 # named impl per primitive family
    secrets: {impl: keyring}

deploy:                     # either an inline deployment document ...
  version: v1
  resources: {...}
  agents: {...}
  runtime: {event_bus: bus}

# ... or definition-level layers, never both:
base_layers:
  - name: base
    priority: 0
    file: base.yaml         # or exactly one of: embed: base.yaml
```

`craft.ParseDefinition` decodes YAML or JSON and validates the static
invariants (`Definition.Validate`):

- `craft.id` and `craft.version` are required.
- `deploy` and `base_layers` are **mutually exclusive**, and one is
  required (`craft: deploy and base_layers are mutually exclusive` /
  `craft: definition requires deploy or base_layers`).
- Each `base_layers` entry needs exactly one of `file` or `embed`
  (`craft: base_layers[0] requires file or embed` or `... declares both`).
- `min_host_version`, when set, is a dotted numeric version with an
  optional prerelease suffix. When the module is a release build
  (`craft.Version` != `"0.0.0"`) and the host is older than the floor,
  validation fails (`craft: requires host version >= X, running Y`). A
  development build still checks the spelling but skips the comparison.

`plugins.builtin` and `plugins.roots` are declarative: the `craft` package
reads only `tool_registry` and `node_targets` during composition; the host
uses `builtin`/`roots` to configure its plugin store. `ui` is the host
application's own frontend delivery, not the plugin UI channel.

## Configuration composition

Layers merge in this order, later layers winning:

1. definition layers: the inline `deploy` document, or each `base_layers`
   entry in declaration order;
2. capability layers (`Layers`), in capability order;
3. craft-level `Options.Layers`;
4. per-runtime layers passed to `OpenRuntime`.

`core/deploy.LoadLayers` performs the merge and keeps provenance. Roots
are `Options.ConfigDir`, `Options.DataDir` and `Options.AppHome` (which
defaults to `DataDir`); `Options.DefinitionDir` anchors definition-level
`file` references and `Options.Assets` resolves definition-level `embed`
references. The active key, values and roots reach factories through the
always-injected `craft.runtime` external
(`RuntimeContext{key, values, config_dir, data_dir, app_home}`, contract
`craft.RuntimeContext`).

### The `${craft:...}` scheme

`${craft:NAME}` resolves, in order:

1. the per-runtime `Values` merged over the craft-level `Options.Values`;
2. the built-in roots `APP_HOME`, `CONFIG_DIR`, `DATA_DIR` and `VERSION`
   (`craft.Version`).

`NAME` is required (`craft: ${craft:} requires a name`) and an unknown
name fails the build (`craft: reference ${craft:NAME} is not defined`).
A capability's `Schemes.Resolver()` is merged on top, so a same-named
capability scheme **wins over the craft built-in** and over the core
defaults. Expansion stays strict and shared with the deploy builder, so
`${env:...}`, `${base}` / `${base:rel}` and `${home}` remain available.

When a plugin host is configured, two more externals are injected:
`craft.plugins` (`tool.Source`) and `craft.pluginhost`
(`craft.PluginHost`). Every injected external is added to the composed
document's `runtime.external_deps`.

## Lifecycle

| Method | Does |
| --- | --- |
| `New(def, opts)` | validates the definition, registers capability factories and service impls, and (with `opts.Plugins`) the plugin node factory. Builds no runtime. |
| `Start(ctx)` | starts the host primitive server and plugin host and installs the plugin revision watcher. Idempotent. Publishes `craft.started`. |
| `OpenRuntime(ctx, key, opts)` | composes layers, builds the runtime, runs every `RuntimeBinder`. Requires `Start`. Publishes `craft.runtime.opened`. |
| `ReloadRuntime(ctx, key, reason)` | recomposes and calls the runtime's transactional `Reload`. Publishes `craft.reload.started` / `.completed` / `.failed`. |
| `ReloadAll(ctx, reason)` | reloads every open runtime; failures are joined with their key. |
| `DrainRuntime` / `Drain` | wait for active turns to finish naturally (one runtime / all). |
| `CloseRuntime(ctx, key)` | drains, closes and removes one runtime. |
| `RegisterAgent` / `UnregisterAgent` | delegate dynamic agent registration to a runtime. |
| `Attach` / `Emit` | subscribe to the craft-plane router / publish on the craft bus. |
| `Close()` | closes every runtime in reverse key order, then the router, bus, plugin host and host server. Idempotent. |

The runtime key (`craft.RuntimeKey`) is application-defined; `craft.DefaultKey`
is `"default"`. An empty key is rejected (`craft: runtime key is
required`), a duplicate is `ErrRuntimeExists` (`craft: runtime already
exists`), and an unknown key is `ErrRuntimeNotFound`. Calling
`OpenRuntime` before `Start` is `ErrNotStarted` (`craft: not started`).

**Reload semantics.** `ReloadRuntime` recomposes the layers into a new
deployment document and hands it to `Runtime.Reload`, which builds a new
deployment generation, rebinds dynamic agents, and atomically swaps it
in. In-flight turns stay on the generation they started on; the retired
generation closes once it drains. Everything outside the document is
preserved: the resource registry, the plugin host and its child
processes, open sessions, and the runtime's craft-plane consumers.

Craft-plane events (exact subjects): `craft.started` (empty
`RuntimeEvent`), `craft.runtime.opened` / `craft.runtime.closed`
(`RuntimeEvent{key}`), `craft.reload.started` / `.completed` / `.failed`
(`RuntimeEvent{key, reason, error}`), `craft.manager.state`
(`ManagerStateEvent`), `craft.group.instance.state`
(`GroupInstanceEvent`). `Reason` is `ReasonManual`, `ReasonConfig`,
`ReasonPlugin` or `ReasonAgent`. Subscribe with
`Attach(ctx, craft.PatternCraft(), sink)` (`craft.>`).

## Capabilities

A capability is any value with `Name() string`; the optional interfaces
are discovered by type assertion, so it implements only what it needs.
Registration order is preserved.

| Interface | Hook | When it runs |
| --- | --- | --- |
| `Registrar` | `Register(*resource.Registry) error` | once in `New`, before anything is built |
| `ServiceRegistrar` | `RegisterServices(*hostmcp.ServiceRegistry) error` | once in `New`, after every `Register` |
| `Layers` | `Layers() []deploy.Layer` | on every composition, after definition layers |
| `Schemes` | `Resolver() *resource.ReferenceResolver` | merged into the `${craft:...}` resolver |
| `HostDecorators` | `HostFactoryDecorators()` | when each runtime is built; reused by reload |
| `ResultHostDecorators` | `ResultHostFactoryDecorators()` | when each runtime is built; reused by reload |
| `HostServices` | `HostServiceSet() hostmcp.Services` | once in `New`, merged into the primitive services |
| `RuntimeBinder` | `BindRuntime(ctx, key, rt) error` | after every successful build *and* reload |

Capabilities are trusted compile-time code, so their errors fail the
build rather than degrade: a nil capability, an empty or duplicate name,
or an error from `Register`, `RegisterServices` or `BindRuntime` aborts
`New` (or `OpenRuntime` for `BindRuntime`). Optional interfaces that are
absent are simply not called — there is no partial-handling path.

Host primitive services are resolved in this precedence: capability
`HostServices` (later capabilities override earlier fields), then
`Options.HostServices` (direct fields override), then
`host_tools.services.<family>.impl` bindings.

## Plugins

A plugin is a directory the host scans:

```text
<root>/<id>/plugin.json   # required, < 1 MiB
<root>/<id>/dist/...      # ui.entry target (optional)
<root>/<id>/server/...    # MCP server files (optional, any language)
<root>/<id>/skills/...    # optional
<root>/<id>/hooks/...     # optional
```

Directories starting with `.` or `_` are skipped. Roots are scanned in
order; a later root shadows an earlier one with the same id, and a user
root shadows a builtin id (`ShadowsBuiltin` + recorded builtin version).
Builtin roots are read-only.

```json
{
  "id": "hello",
  "name": "Hello Plugin",
  "version": "0.2.0",
  "minHostVersion": "0.1.0",
  "permissions": ["mcp:provide", "skills:provide"],
  "ui": {"entry": "dist/index.js"},
  "mcp": {"command": "python3", "args": ["server/main.py"],
          "env": {"LOG_LEVEL": "info"}, "required": false},
  "skills": ["skills"],
  "hooks": ["hooks/on_start.json"],
  "nodes": [{"type": "echo", "tool": "node_echo", "timeout": "30s"}]
}
```

`id` must match `^[a-z0-9][a-z0-9._-]{0,63}$`. Limits: skills ≤ 32,
hooks ≤ 16, MCP servers ≤ 16, nodes ≤ 64; `mcp.env` ≤ 32 entries (key ≤
128, value ≤ 4 KiB); paths ≤ 256 chars and confined to the plugin root.
`mcp.transport` is `stdio` (default, needs `command`) or `http` (needs
`url`); a path-bearing `command`/`args` is resolved against and confined
to the plugin root, while a bare command goes through `PATH`. The legacy
`mcpServers` and top-level `entry` aliases are accepted; the removed
`kraft` and `tools` fields are rejected with an upgrade hint.

### Permissions

| Permission | Authorizes |
| --- | --- |
| `mcp:provide` | the `mcp` server section |
| `skills:provide` | the `skills` list |
| `hooks:provide` | the `hooks` list |
| `nodes:provide` | the `nodes` section |
| `secrets:auth` | `secret_get` / `secret_set` / `secret_delete` |
| `inference:write` | `inference_upsert` / `inference_remove` |
| `telemetry:export` | `telemetry_configure` / `telemetry_disable` |
| `sessions:import` | `session_import` / `session_imported_sources` |
| `host:open_url` | `open_url` |
| `events:emit` | `emit_event` |
| `ui:webview` | the UI bundle (the shell gates it) |
| `storage:kv` | the plugin KV store (the shell binds it) |

An unknown permission is rejected, not ignored. The gate is **fail-closed
at contribution time**: a plugin that declares a gated section without
its permission still loads and enables, but the section is dropped:

- `mcp` without `mcp:provide`: no child process starts, no host token is
  minted, and a node bound to it fails with the missing grant;
- `skills` without `skills:provide`: `SkillRoots` omits the directories;
- `hooks` without `hooks:provide`: `HookFiles` omits the files;
- `nodes` without `nodes:provide`: no node resource or engine dep is
  synthesized;
- `ui` and `storage` carry no gate inside craft — the shell gates the
  bundle and binds the KV, while `Host.KV` is the host accessor.

Disabling a plugin withdraws its contributions even though its
permissions are unchanged; re-enabling restores them.

### Tools

Enabled plugins share one `plugin.ToolSet`, injected into every runtime
as the `craft.plugins` external (`tool.Source`). Tools are namespaced:
`ToolPrefix(id)` is the id reduced to `[a-z0-9_-]` and suffixed with
`__`, so plugin `hello`'s `echo` tool is `hello__echo`. The set fans
additions and removals out to every attached registry (one per runtime
generation), so adding or removing a plugin reaches live catalogs without
reopening runtimes.

Tools reach agents and graph nodes through a mount point added during
composition as a `tool.plugins: craft.plugins` dependency:

- on the resource named by `plugins.tool_registry`, if set — it must
  exist (`craft: plugins.tool_registry resource "X" not found`) and, to
  take effect, declare a `tool` `Many` dependency (e.g. a
  `tool.Assembly` or `tool.Registry`); or
- on every resource whose factory declares a `tool` `Many` dependency,
  when `tool_registry` is empty.

A capability can also read the plugin host at build time via the
`craft.pluginhost` external (`SkillRoots`, `HookFiles`, `Entries`,
`CallTool`). Disable/update drops the plugin's tools from every registry
and waits for in-flight calls before the source closes.

## Plugin graph nodes

Each manifest `nodes` entry becomes an executable graph node: craft
synthesizes a `graph.NodeType` / `mcp` deployment resource whose settings
carry `type` (namespaced `<plugin-id>.<node-type>`), `plugin`, `tool` and
an optional `timeout`. When `plugins.node_targets` is set, every named
agent receives an engine dependency `node_type.<id>.<type>` pointing at
that resource, and each target must exist
(`craft: plugins.node_targets agent "X" not found`).

The node handler calls the plugin's MCP tool with
`{node: {id, type, graph}, config}` and applies the returned
`{"writes": {...}}` object to the board. The method never appears in the
model's tool catalog; the node runs in the plugin subprocess and each
call is bounded by the node `timeout` (default 30s) and the run context.
`nodes` is gated by `nodes:provide`; the tool it calls needs
`mcp:provide`, so a node plugin usually declares both. A node backed by a
plugin lacking `mcp:provide` is still synthesized and fails at call time
with the missing grant rather than disappearing silently.

## hostmcp: host primitives

`craft/hostmcp` is a local MCP server that exposes host primitives to
plugins as tools. It transports over streamable HTTP bound to `127.0.0.1`
on an ephemeral port at `/mcp`, started by `Craft.Start` and stopped last
by `Craft.Close`.

- **Authentication.** Each plugin gets its own bearer token, minted when
  its source is created and revoked on disable, update or close. The
  token is passed as `Authorization: Bearer` and injected into the plugin
  process as `CRAFT_PLUGIN_TOKEN`, alongside `CRAFT_HOST_MCP_URL`. A
  missing or unknown token is `401`; a non-loopback `Host` header or any
  request carrying an `Origin` is `403` (DNS-rebinding defense); a token
  over its rate limit (default 20 requests/second) is `429`.
- **Scoping.** A token maps to `{pluginID, grants}` where grants are the
  manifest permissions. A tool whose grant the caller lacks is not
  exposed to that caller; calls dispatch as identity → grant → execute →
  audit.
- **Registry and handshake.** `Registry.Register` requires a name and a
  handler and rejects duplicates. `host_about` (protocol version 1) is
  always registered and reports the exposed primitives, the disabled
  ones, and the caller's missing grants; call it at startup and fail fast
  when a needed primitive is absent.
- **Services.** The v1 set is backed by `hostmcp.Services`; each nil
  field removes its primitives from the exposed set (fail closed).
- **Audit.** Every call yields one `AuditEvent{plugin_id, tool, outcome,
  duration}` with no payloads, forwarded to telemetry as `hostmcp call
  plugin=... tool=... outcome=...`.

### v1 primitives

| Tool | Grant | Backed by |
| --- | --- | --- |
| `host_about` | — | always available |
| `secret_get` / `secret_set` / `secret_delete` | `secrets:auth` | `SecretService` |
| `workspace_current` | — | `ContextService` |
| `open_url` | `host:open_url` | `BrowserService` |
| `inference_upsert` / `inference_remove` | `inference:write` | `InferenceService` |
| `session_import` / `session_imported_sources` | `sessions:import` | `SessionService` |
| `telemetry_configure` / `telemetry_disable` | `telemetry:export` | `TelemetryService` |
| `emit_event` | `events:emit` | `EventService` |

`host_tools.disable` names primitives that are never exposed (reported
`disabled` in `host_about`). `host_tools.services.<family>.impl` selects a
named implementation registered by a capability through
`ServiceRegistrar`. The families are `secrets`, `workspace`, `browser`,
`inference`, `sessions`, `telemetry` and `events`, each bound to its
interface. A missing impl, an unknown family, or an impl of the wrong
type fails `craft.New`.

## craft/manager

`craft/manager` owns the process-level lifecycle of one Craft: definition
lookup, profiles and paths, the single-instance lock, migrations, a state
machine, signal-aware `Run`, and the blue-green `Replace`.

- **Definition lookup.** `LocateDefinition` tries the explicit path,
  `$CRAFT_DEFINITION`, the working directory, the executable directory,
  then `<UserConfigDir>/craft/craft.yaml`; a miss is `craft manager:
  craft.yaml not found`.
- **Paths and lock.** `Paths{ConfigDir, DataDir, AppHome}`. With `Lock`
  set, `DataDir` is required and the manager takes
  `<DataDir>/craft.lock` (or `craft-<profile>.lock`). A second instance is
  `craft manager: another instance holds <path>`; a stale lock left by a
  crash must be removed manually.
- **State machine.** `idle → starting → running → draining → stopping →
  stopped`, with `failed` on a failed start. `Subscribe` observes every
  transition and each running Craft emits `craft.manager.state`.
- **Run.** Starts the Craft, runs the shell `Runner` until it returns or
  the context ends, retries the runner per `RestartPolicy`, then stops.
- **Replace.** Builds a fresh Craft with the same paths, starts it, swaps
  the pointer atomically, then drains and closes the previous Craft. It
  requires a running Craft (`craft manager: Replace requires a running
  Craft`) and is the path for changes that need a new binary or a new
  capability set; everything inside the document is covered by
  `ReloadRuntime`.

### manager.Group

`Group` hosts several isolated managers in one process. Each instance
keeps its own paths, lock, Craft and state machine; sharing happens only
through the `Spec.Build` closure. `InstanceID` must match
`^[a-z0-9][a-z0-9._-]{0,63}$`; with a `GroupOptions.Root`, missing paths
default to `<Root>/<id>/{config,data}`. `MaxInstances` (default 32) and
`StartTimeout` (default 1 minute) bound the group, and exceeding the cap
is `craft manager group: max instances reached`. `Add` / `Remove` / `Get`
/ `Instances` / `StartAll` / `StopAll` / `Run` / `Close` manage the set,
and each instance re-emits its state as `craft.group.instance.state`.

**In-process instances are not a security boundary.** They share the Go
runtime and memory, so one capability's panic or memory fault affects the
whole process. Use a group for mutually trusting instances (profiles,
several apps of one operator); untrusted tenants need one process per
instance.

## Failure modes to expect first

- Definition: `craft: definition id is required`, `craft: definition
  version is required`, `craft: deploy and base_layers are mutually
  exclusive`, `craft: definition requires deploy or base_layers`.
- Capabilities: `craft: duplicate capability "app"`, `craft:
  capability[0] is nil`, `craft: capability[0] has an empty name`,
  `craft: capability app register: ...`.
- Service bindings: `craft: host_tools.services.secrets impl "x" is not
  registered`, `craft: host_tools.services has unknown family "x"`,
  `craft: service secrets.x is *T, want SecretService`.
- Lifecycle: `craft: not started` (`ErrNotStarted`), `craft: runtime
  already exists` (`ErrRuntimeExists`), `craft: runtime not found`
  (`ErrRuntimeNotFound`), `craft: runtime key is required`.
- Mounting: `craft: plugins.tool_registry resource "tools" not found`,
  `craft: plugins.node_targets agent "assistant" not found` — mount points
  are validated against the composed document.
- Plugin manifest: `plugin hello: unknown permission "x"`, `plugin:
  invalid id "X"`, `plugin hello: the kraft field is no longer supported;
  ...`, `plugin hello: mcp and mcpServers are mutually exclusive`.
- Plugin gate: a call to a plugin whose `mcp` section was dropped is
  `Forbidden` (`the mcp section is ignored without the mcp:provide
  permission`); a plugin with no server answers `NotFound` (`not
  configured`).
- Install: `plugin install: hello version 0.1.0 is not newer than 0.2.0`;
  rollback without a snapshot is `plugin install: no rollback snapshot
  for "hello"`. See the `flowcraft-config` reference card for the full
  error → cause table.

## Status

Craft lives in the workspace and is **not yet released**: no `craft/vX.Y.Z`
tag exists, and `craft.Version` stays `"0.0.0"` for development builds. The
host primitive set is **v1** and its tool names are a wire contract. The
plugin install / update / rollback *surface*
(feeds, channels, UI orchestration) and the desktop / shell side are not
part of craft; the app owns them. Do not assume behaviour beyond what the
code and the reference cards describe.

## Minimal example

`craft.yaml`:

```yaml
craft:
  id: hello
  name: Hello
  version: 0.1.0

deploy:
  version: v1
  resources:
    bus:
      kind: event.Bus
      impl: memory
  runtime:
    event_bus: bus
```

```go
package main

import (
	"context"
	"log"
	"os"
	"path/filepath"

	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/craft"
)

// appCapability registers the factories the deploy document references.
type appCapability struct{}

func (appCapability) Name() string { return "app" }

func (appCapability) Register(registry *resource.Registry) error {
	return event.Register(registry)
}

func main() {
	dir, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "craft.yaml"))
	if err != nil {
		log.Fatal(err)
	}
	def, err := craft.ParseDefinition(raw)
	if err != nil {
		log.Fatal(err)
	}
	c, err := craft.New(def, craft.Options{
		ConfigDir:     filepath.Join(dir, "config"),
		DataDir:       filepath.Join(dir, "data"),
		DefinitionDir: dir,
		Capabilities:  []craft.Capability{appCapability{}},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		log.Fatal(err)
	}
	if _, err := c.OpenRuntime(ctx, craft.DefaultKey, craft.RuntimeOptions{}); err != nil {
		log.Fatal(err)
	}
	log.Printf("craft %s running", def.Craft.ID)
}
```

Copy `assets/minimal-craft` from the `flowcraft-config` skill for a fuller
starting point: an inline deploy with one agent, explicit `ui`, `plugins`
and `host_tools` sections, and a sample plugin directory. For a runnable host
application that exercises keyed runtimes, per-runtime values and layers, tools,
reload, plugin hot-plug and the host primitives, see `examples/anvil` and its
README.
