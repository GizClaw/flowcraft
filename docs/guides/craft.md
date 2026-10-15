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
4. per-runtime layers passed to `OpenRuntime` (or to `ReloadRuntimeWith`
   for a later generation).

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
| `New(def, opts)` | validates the definition, registers capability factories and service impls, and (with `opts.Plugins`) the plugin node factory and the UI registry. Builds no runtime. |
| `Start(ctx)` | starts the host primitive server and plugin host and installs the plugin revision watcher. Idempotent. Publishes `craft.started`. |
| `OpenRuntime(ctx, key, opts)` | composes layers, builds the runtime, runs every `RuntimeBinder`. Requires `Start`. Publishes `craft.runtime.opened`. |
| `ReloadRuntime(ctx, key, reason)` | recomposes and calls the runtime's transactional `Reload`. Publishes `craft.reload.started` / `.completed` / `.failed`; reports no generation — `ReloadRuntimeWith` is the entry point that returns one. |
| `ReloadRuntimeWith(ctx, key, opts, reason)` | `ReloadRuntime` with fresh `RuntimeOptions`: the next generation is composed and built with them, and once it is serving and bound they are what the key keeps. Returns that generation's `*runtime.ReloadResult`; a failed reload returns none, and its error names the generation serving instead. |
| `ReloadAll(ctx, reason)` | reloads every open runtime; failures are joined with their key. A closed Craft has nothing to reload and returns nil. Reports no generation: one per key is not one answer. |
| `DrainRuntime` / `Drain` | wait for active turns to finish naturally (one runtime / all). |
| `CloseRuntime(ctx, key)` | drains, closes and removes one runtime. |
| `RegisterAgent` / `UnregisterAgent` | delegate dynamic agent registration to a runtime. |
| `SyncAgents(ctx, key, decls, opts...)` | reconcile a runtime's dynamic agents with a declaration set (register / replace / unregister); never reloads. `craft.WithAgentRemoveTimeout` replaces the runtime's configured bound on one drain. |
| `Attach` / `Emit` | subscribe to the craft-plane router / publish on the craft bus. |
| `Close()` | closes every runtime in reverse key order, then the UI registry, router, bus, plugin host and host server. Idempotent. |
| `UI()` | the plugin UI registry (`*ui.Registry`), nil without a plugin host. |

The runtime key (`craft.RuntimeKey`) is application-defined; `craft.DefaultKey`
is `"default"`. An empty key is rejected (`craft: runtime key is
required`), a duplicate is `ErrRuntimeExists` (`craft: runtime already
exists`), and an unknown key is `ErrRuntimeNotFound`. Calling
`OpenRuntime` before `Start` is `ErrNotStarted` (`craft: not started`).

**Reload semantics.** `ReloadRuntime` recomposes the layers into a new
deployment document and hands it to `Runtime.Reload`, which builds a new
deployment generation, rebinds dynamic agents, and atomically swaps it
in. In-flight turns stay on the generation they started on; the retired
generation closes once it drains. Generation ids are allocated per
attempt — including attempts that fail before the swap — so they are
monotonic but not contiguous. A reload whose bind fails is rolled back
to the document it was serving under a *fresh* id, which the error
names: what a caller was last returned is not what serves after a
failure. Everything outside the document is preserved: the resource
registry, the plugin host and its child
processes, open sessions, and the runtime's craft-plane consumers.
Reloads are serialized with each other and with `SyncAgents` through the
lifecycle lock: an agent pass never observes a reload of this Craft in
flight.

**Reload options.** The `RuntimeOptions` a key was opened with are kept
with the key, and a reload recomposes from them, so a reload carries a
changed document but the same per-runtime inputs. `ReloadRuntimeWith`
replaces them — layers, `Values`, externals, host decorators — and the
replacement becomes the options the key holds once its generation is
serving and bound, so a later `ReloadRuntime` or `ReloadAll` recomposes
with it too. A reload that fails leaves the key on the options and
inputs it was serving with; the generation is the half that moves, and
the error names what serves. An unknown key is `ErrRuntimeNotFound`
(`craft: runtime not found`) and a closed Craft is `ErrCraftClosed`
(`craft: closed`) for both entry points. `CloseRuntime` and `Close` do
not wait for a reload in flight — they take the runtime out from under
it — and such a reload fails rather than report a generation nothing
serves: `ErrRuntimeNotFound` for a closed key, `ErrCraftClosed` for a
closed Craft.

Craft hands the replacement to the runtime as `runtime.ReloadOption`s:
the composed externals (`WithReloadExternals`), the `${craft:...}`
resolver built from the merged `Values` (`WithReloadResolver`) and the
host factory decorations (`WithReloadHostFactory` /
`WithReloadResultHostFactory`). Every channel is passed on every reload,
so the generation is built with exactly the options at hand; the
decorator channel carries nil when the options have none, which is how a
reload drops the previous generation's decoration instead of leaving it
in place.

Craft's per-runtime inputs are therefore not frozen at open time. A shell
that owns a deploy overlay on disk — one it re-reads on every settings
save — hands craft the new layers through `ReloadRuntimeWith` instead of
`CloseRuntime` + `OpenRuntime`, which would interrupt the in-flight turns
the generation swap exists to protect.

Craft-plane events (exact subjects): `craft.started` (empty
`RuntimeEvent`), `craft.runtime.opened` / `craft.runtime.closed`
(`RuntimeEvent{key}`), `craft.reload.started` / `.completed` / `.failed`
(`RuntimeEvent{key, reason, error}`, with no generation id: the id a
swap installed is `ReloadRuntimeWith`'s return value), `craft.manager.state`
(`ManagerStateEvent`), `craft.group.instance.state`
(`GroupInstanceEvent`), and `craft.ui.changed`
(`ui.ChangedEvent{revision}`, published by the UI registry). `Reason`
is `ReasonManual`, `ReasonConfig`, `ReasonPlugin` or `ReasonAgent`.
Subscribe with `Attach(ctx, craft.PatternCraft(), sink)` (`craft.>`).

### Dynamic agents

`SyncAgents` reconciles one runtime's *dynamically registered* agents
with a declaration set — `[]craft.AgentDecl{Name, Def, Rev}` — without
reloading the generation:

- new names are registered, a changed `Rev` replaces the agent
  (unregister + register), a name the set no longer declares is
  unregistered, and an unchanged `Rev` whose agent is live is left
  alone. The set is the full desired set: an empty set removes
  everything a previous pass applied.
- `Rev` is the application-owned declaration revision and the only input
  to the diff: fold whatever makes the declaration what it is (the
  definition, its settings, the resources it references) into it — a
  content hash is the natural choice. An empty `Rev` is rejected, as are
  an empty or duplicated name.
- the applied set is remembered per Craft and runtime key, so it
  survives reloads (a reload re-binds the dynamic agents itself) and a
  runtime reopen. A record is trusted only while `Runtime.Agent` still
  has the name; a name that is live but unrecorded — a direct
  `RegisterAgent` call — is replaced, because the declaration set is
  authoritative over the runtime's live view.
- a per-declaration failure does not abort the pass. Failures come back
  per name in `AgentSyncReport.Errors` and joined as the returned error;
  a failed declaration is not recorded, so the next pass retries it even
  with an unchanged `Rev` — the repair path for declarations that arrive
  from outside the application.
- every removal a pass performs — a name the set dropped, and the live
  agent a replacement replaces — drains the agent's active turns bounded
  by the pass's removal bound: `craft.WithAgentRemoveTimeout` when
  given, otherwise the runtime document's
  `runtime.agents.remove_timeout` when set, otherwise
  `craft.DefaultAgentRemoveTimeout` (30s). The bound is there because
  the pass holds the lifecycle lock: a reload, a close and the next pass
  all queue behind that drain. A removal that runs out of time fails its
  declaration — the agent stays live, its record stays, and the next
  pass retries — and a non-positive bound is rejected before anything
  mutates. Lower it in applications whose turns are short; there is no
  unbounded setting.
- deployed agents are never touched: a declaration whose name the
  deployment document declares is an error, and deployment documents
  must not declare names the coordinator manages.
- `SyncAgents` triggers no reload — call `ReloadRuntime` with
  `craft.ReasonAgent` when the document itself must change. It holds the
  same lifecycle lock as reloads, and `RuntimeBinder` callbacks and
  event sinks run on paths that may hold that lock: do not call
  `SyncAgents`, `ReloadRuntime` or `ReloadAll` synchronously from them.

Applications whose dynamic agents need a tool assembly do not pass one
per declaration: declare a fallback in the deployment document
(`runtime.dynamic_catalog.tools.default: <tool.Assembly resource>`) and
every dynamically registered agent runs with it. A runtime whose dynamic
catalog has no default rejects a declaration per name
(`... needs WithToolAssembly`).

The declaration *source* belongs to the application: craft takes the set
it is handed and keeps no schema of its own (per-agent files, versions
and CRUD tools stay on the application side). The migration from
imperative per-agent bookkeeping is "read the declarations once at cold
start and once after each reload, call `SyncAgents`": the
per-generation known-set handoff the reference implementation needed
disappears, because the records live on the Craft and a reload re-binds
what they describe.

The removal bound is document configuration: the runtime's
`agents.remove_timeout` is the default bound for a dynamic agent
removal, so it applies to every `UnregisterAgent` call on that runtime —
`SyncAgents` passes included (see [runtime.md](runtime.md)). Craft
resolves one removal's bound in this order:

1. `craft.WithAgentRemoveTimeout` on the pass, when given;
2. the runtime document's `runtime.agents.remove_timeout`, when set;
3. `craft.DefaultAgentRemoveTimeout` (30s).

```yaml
runtime:
  event_bus: events
  agents:
    remove_timeout: 5s
```

Because it is an ordinary `runtime:` key, it is per-runtime: layers and
`RuntimeOptions.Layers` override it per runtime, a reload carries a
changed value, `${...}` references resolve, and the strict decode
rejects a misspelled key at build time. A bound computed per call still
goes through `craft.WithAgentRemoveTimeout`.

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

### Install, update, uninstall

A plugin arrives as a directory or as a zip package. Both go through the
same pipeline — `Store.Install(ctx, dir)` and
`Store.InstallZip(ctx, zipPath)` — so a package gets the same manifest
validation, the same version rule, the same snapshot and the same
restore:

- Installing over an installed plugin requires a version that is
  **strictly newer**; the old copy is snapshotted to
  `<root>/.backups/<id>` first and put back if the copy fails. A fresh
  install that fails leaves nothing behind. `Store.Rollback(ctx, id)`
  restores the snapshot.
- The install lands in the same writable root the scan prefers (the
  last one configured), so the plugin it creates is the plugin the scan
  serves. An id that exists only as a builtin gets a user copy that
  shadows it; that copy must be newer than the builtin, and may not be
  older than a builtin the application has since updated.
- A zip package may carry the files at the archive root or under a
  single top-level directory. Everything else about the archive is
  refused: absolute names, `..` elements, backslashes, symlinks and
  anything that is not a file or a directory. One entry may unpack to
  `plugin.MaxZipEntryBytes` (64 MiB) and the whole package to
  `plugin.MaxZipBytes` (256 MiB), read from the headers — archive/zip
  refuses to read a stream that runs past its declared size — and the
  temporary tree is removed whatever happens.

`Store.Uninstall(ctx, id, opts)` removes a plugin: its directory, its
snapshot, its enable state and — by default — its KV file
(`<state>/kv/<id>.json`) and data directory (`<data>/<id>`).
`UninstallOptions{KeepKV: true}` / `{KeepData: true}` keep them, for
content a reinstall should still find. A builtin cannot be uninstalled
(disable it instead); removing the user copy that shadows a builtin
uncovers the builtin again. `Host.Uninstall` asks the store first
(`Store.CheckUninstall`: a builtin, or an id that is not installed, is
refused before any process is killed), stops the plugin (in-flight calls
drain, the source closes), and then uninstalls; a store that refuses after
the stop starts a plugin that was running again, so the refusal costs it a
restart instead of leaving a plugin uninstalled while its process runs on.
`Store.Uninstall` on its own stops nothing. State the application keys by
plugin id — secrets, inference profiles, telemetry — is not craft's to
delete and stays.

**Host-side changes.** `Host.Install(ctx, source)`,
`Host.UpdateFrom(ctx, id, installer)` and `Host.Rollback(ctx, id)` run one of
those store pipelines and add the process half of it — stop, change, start —
so a running plugin never reads files that changed under it:

- The process is stopped before the change (the change replaces or removes
  the directory it reads) and started again from what the store now holds.
- A change the store refuses — a version rule, a checksum, a download the
  installer declined — leaves the installed version in place, and it is
  started again before the call returns: what the refusal costs a running
  plugin is a restart, not a stopped plugin.
- A change that landed but whose version cannot come up is undone where the
  store can undo it (the snapshot `Store.Rollback` restores) and the version
  that ran before is started again; the caller sees why the new one failed.
  `Host.Rollback` is the one change that cannot undo itself — its swap
  consumes the version it replaced — so a rollback whose version does not
  come up leaves the plugin stopped with its record disabled, and installing
  a package is what repairs it.
- A version no attempt can bring up leaves the plugin stopped with its record
  disabled, including the case where the stop itself could not finish: the
  drain gives up at `HostOptions.DrainTimeout` and the source closes anyway,
  so the record that says enabled goes with the process that is gone.
  "Enabled" about a plugin with no process is the one state the host does not
  leave behind.
- A change does not turn a plugin on: one that was not running is left that
  way, including one whose process gave up on its own — that loss was
  reported, and the record is the application's to change.
- An id that was not installed at all is recorded disabled by the install
  that creates it. A fresh id has no enable record, and a store with no
  record for an id reads it enabled: without that write the entry would say
  enabled about a plugin with no process behind it — the state above — from
  the install until something else changed it. `Host.SetEnabled` is what
  starts it.

Each call holds the plugin's entry lock, so an enable or another change of the
same plugin waits for it rather than racing it. `Store.Install`,
`Store.UpdateFrom`, `Store.Rollback` and `Store.Uninstall` are the same
pipelines without the process half: calling them straight is the application's
own to keep in step, and nothing in craft starts or stops a process for them.

**Remote updates.** A manifest may declare where to look for a newer
version:

```json
"update": {"url": "https://example.test/plugins/hello.json"}
```

The URL must be an absolute `http(s)` URL without credentials or a
fragment (≤ 2048 bytes); the scheme is *checked*, not restricted —
https-only is transport policy, and transport belongs to the
application. `plugin.Installer` is the seam for it: the application
implements `Check(ctx, url)` (fetch the update manifest, see
`plugin.UpdateInfo`) and `Fetch(ctx, info)` (download the package,
return its path and a cleanup), deciding proxies, credentials and
network policy itself. `Store.UpdateFrom(ctx, id, installer)` then runs
the whole pipeline: fetch the manifest → validate the version, the
download URL and the `sha256:<64 hex>` checksum → require the announced
version to be newer than the installed one → fetch → verify the package
against its checksum (≤ 256 MiB) → require the package to hold the same
plugin at the announced version → install it under the version rule and
rollback above.

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
- `hooks` without `hooks:provide`: `HookSources` omits the files;
- `nodes` without `nodes:provide`: no node resource or engine dep is
  synthesized;
- `ui` and `storage` carry no gate inside craft — the shell gates the
  bundle and binds the KV, while `Host.KV` is the host accessor. The
  registry serves the bundle and reports the grant in its entries, so
  the shell refuses to load what the plugin did not ask for.

Disabling a plugin withdraws its contributions even though its
permissions are unchanged; re-enabling restores them.

### UI delivery

craft delivers bundles and lifecycle; the shell decides how a bundle
runs. `Craft.UI()` returns the registry (`craft/ui`), or nil when the
Craft has no plugin host:

```go
registry := craft.UI()                        // nil without a plugin host
entries, err := registry.Entries()            // what there is to load
assets, ok := registry.Assets(entries[0].ID)  // where to read it from
```

`Entries()` lists every valid plugin — enabled or not — sorted by id,
with `id`, `name`, `version`, `entry`, `permissions`, `enabled` and the
`revision` the read started at. Invalid plugins are not enumerated:
scan errors belong to the management view (`plugin.Host.List`).
`Assets(id)` returns an `fs.FS` rooted at the directory holding the
plugin's `ui.entry`, so `dist/index.js` is `index.js` and the chunks
beside it resolve the way the bundle's own relative imports do. A
plugin without `ui.entry`, an unknown id and a host that cannot serve
assets all report false — the registry probes for that half at run
time, so `craft.PluginHost` stays the narrow contract applications
implement.

The bundle view is re-validated on every open, because a plugin
directory is user-writable and can change under a running shell: a path
must be slash-separated, relative and clean (`..`, absolute paths and
backslashes are `fs.ErrInvalid`); a symlink leaving the bundle is
`plugin.ErrAssetEscape` while one that stays inside is followed; a
device, socket or fifo is refused; and no single asset may exceed
`plugin.MaxAssetBytes` (10 MiB), checked when the file is opened and
again while it is read.

**Events.** `craft.ui.changed` (`ui.SubjectChanged`) carries
`ui.ChangedEvent{revision}` and is published on the craft bus whenever
the plugin store's revision moves — enable, disable, install, update
and rollback all end there. Subscribe to it like any other craft event:
`craft.Attach(ctx, event.Pattern(ui.SubjectChanged), sink)`. The
registry starts watching in `New`, so a shell that subscribes before
its first `Entries()` call cannot miss a change, and the contract on
each event is:

1. destroy every plugin scope from the previous round;
2. read `Entries()` again, and record the revision it carries;
3. load each enabled bundle, handing the plugin's id, version and
   permissions to the JS host.

Two asset roots, on purpose: `Assets` / `plugin.Host.AssetFS` read
inside the bundle, bundle-relative, which is what a JS loader needs,
while `plugin.Store.Asset(id, rel)` materializes one file relative to
the plugin directory for assets that live outside the bundle (a pet
pack, say). Both refuse a symlink out of the plugin tree.

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
`craft.pluginhost` external (`SkillRoots`, `HookSources`, `Entries`,
`CallTool`). `HookSources` hands each plugin hook file to a command hook
runner together with the two facts it needs — the plugin directory the
commands run in, and the untrusted marking that strips content fields
from the payload ([External Hooks](hooks.md)). Disable/update drops the
plugin's tools from every registry and waits for in-flight calls before
the source closes.

`plugin.Host.OnStopped` reports every stop with a `plugin.StopReason`:
`plugin.StopRequested` for a stop the application asked for (disable,
uninstall, close), `plugin.StopRolledBack` for an enable the host undid
because the plugin could not come up, and `plugin.StopFailed` when a
plugin's MCP server died on its own and the reconnect could not start
the command again. A `StopFailed` loss leaves the entry alone — enabled,
with its tools still projected — so the application decides whether to
disable or uninstall the plugin, and knows why its tool calls are
failing in the meantime. Re-enabling is the other answer, and it is a
start: the source that gave up is retired and a fresh one opened and
waited for, and an enable that fails again reports the failure and
leaves the record disabled. Transient losses that reconnect in place are
not reported, so a self-healing plugin does not flap the observer; nor
is a loss that races the host's own close, or one the enable in flight
rolled back on, where the rollback is the report. A loss that arrives
while an enable is waiting is held for that call: if the enable stands,
the loss is reported once the call ends, so a plugin is never recorded
enabled with a server that gave up and nothing said about it. A loss that
arrives while the host is stopping that same process is one event with
that stop, reported once, as whichever side recorded it first — the stop
the application asked for, or the loss that got there first — while the
host's own half runs either way: the process leaves the live set, and
the token minted for it is revoked.

Enabling is an answer rather than a spawn: `plugin.Host.SetEnabled`
waits for the plugin's MCP servers, bounded by
`HostOptions.EnableTimeout` (`plugin.DefaultEnableTimeout`, 10s; a
negative value keeps the old spawn-and-return behavior). An enable whose
command is gone, not executable, or refused by the peer reports that
failure and is rolled back — the process is stopped, the entry is left
disabled, and `OnStopped` says `StopRolledBack` — instead of recording an
enabled plugin with no process. A server that is merely not there yet (a
remote endpoint that is down, a child that keeps dying during startup)
is not a failure of enable: the wait gives up at the timeout, the entry
is recorded enabled, and the source keeps reconnecting. A plugin whose
enable is in flight is what narrates its own failure, so the failure the
rollback answers is not reported a second time through `OnStopped`.
The wait covers every server the manifest declares: `mcp.required` marks
a server the plugin cannot run without for the MCP layer's own
reporting, and says nothing about the enable — a plugin whose declared
server cannot start is refused whichever way that flag is set.
`Host.Start` does not wait: it replays what a previous session recorded,
and an application's startup must not depend on a plugin's process.

Disabling is a stop plus the record: the process drains and closes, and the
entry is written disabled. A stop that runs past `HostOptions.DrainTimeout`
closes the source anyway and reports it, and the entry goes disabled with the
process that is gone — the record never says enabled about a plugin the host
is not running.

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
`disabled` in `host_about`); naming `host_about` itself fails `craft.New`,
because it is how a plugin learns the primitive set and carrying the
disabled ones is its answer. `host_tools.services.<family>.impl` selects a
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
  craft.yaml not found`. `Options.Definition` short-circuits the whole
  lookup: a non-empty byte slice is parsed in place (and `DefinitionPath`
  is ignored), which is the path for a shell whose definition is embedded
  in the binary. Bytes go through the same `ParseDefinition` validation a
  file does.
- **Definition anchor.** The directory a definition's own file
  references resolve against is the directory the definition was found
  in — the located path, not `Options.DefinitionPath` and not the
  working directory. `base_layers` entries with a file source read from
  there, and so does the default loader's `${base:...}` expansion (a
  shell-supplied `Options.Loader` anchors wherever it says). Layers the
  shell passes itself — `Options.Layers`, `OpenRuntime` layers — carry
  their own `BaseDir` and are not anchored here. An embedded definition
  has no directory of its own, so `Options.DefinitionDir` names it;
  empty means the working directory. `Replace` builds with whichever
  anchor `Start` resolved: the replacement definition has no directory
  of its own.
- **Paths and lock.** `Paths{ConfigDir, DataDir, AppHome}`. With `Lock`
  set, the manager takes `<DataDir>/craft.lock` (or
  `craft-<profile>.lock`), so a `DataDir` is required — unless `LockPath`
  names the file explicitly, which is how a shell that already owns a
  lock spelling (a state root's `gui.lock`, the file another instance of
  it locks and raises through) points the manager at that file instead of
  leaving a second, unaware lock beside it; the derived name is untouched
  while `LockPath` is empty. `LockOptions` are passed through to
  `core/utils/lock`: `WithVersion` and `WithEndpoint` land in the holder
  record, so `OnLockHeld` — and a launch that reads the record to raise
  the holder — can name its build and reach it. A second instance is
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

## craft/pool

`craft/pool` is the optional package that keeps **one member per key
alive across generations**: which key gets which member, when an
invalidated member is retired, who assembles its replacement, and how
callers wait that swap out. It is not built into `Craft`: the value of a
key is the application's — a Craft runtime, a connection, whatever it
pools — and the pool is handed a `Spec[T]` that says how one is
assembled, when it is busy, and how its teardown finishes. Everything
the pool asks is a hook, so `T` needs no interface: a nil `Busy` means
never busy, a nil `WaitClosed` means `Close` tears down synchronously,
a nil `Wanted` means every key is wanted.

```go
type Key struct{ Kind, ID string } // two identifiers, one namespace each

func New[T any](spec Spec[T]) (*Pool[T], error)
// Ensure / Acquire / Current / Stale / Release / Settle
// Invalidate / InvalidateKind / InvalidateAll / Close
// ScheduleReplacement / ReplacementArmed / Do
```

- **Two entrance points.** `Ensure` is the cheap read path, called once
  per unit of work: it answers with the pooled member — including one
  that was invalidated but is still serving its last work on the old
  generation — or assembles one, once any member retiring for the key
  has finished teardown. It takes no reference. `Acquire` checks a
  member out: it waits out a retiring member's teardown instead of
  handing out one on its way out, concurrent callers for one key share a
  single assembly (error included), and the caller owes exactly one
  `Release`. What a reference buys is the member's stay rather than its
  life: the last `Release` retires a member its key was invalidated for,
  and until that release comes the member is not closed under the caller
  that holds it — an idle one either — while a member nothing
  invalidated stays pooled and serves the next caller.
- **A key names two things.** `Kind` is the application's namespace
  (`workspace`, `app`, …), so the same id in two kinds is two keys and
  neither kind can answer for the other. A blank part is not a key:
  `Ensure` and `Acquire` refuse it with `ErrNoKey`, the reads answer
  with nothing, and the retirements ignore it.
- **Invalidation defers to live work and to a caller.** `Invalidate`
  marks the key's member stale. An idle member that nobody holds closes
  immediately; a member with work in flight, or one a caller checked
  out, stays pooled and keeps serving on the old generation until the
  last unit ends (`Settle`, once no reference is left) or the last
  `Release` takes it — a second generation is never assembled under a
  live unit or under a caller that is using one. A `Release` is the
  application's word that its caller is done, so it retires a stale
  member whatever `Busy` reports, and the application owes it after its
  work rather than before it. `InvalidateKind` drops one kind and
  nothing else; `InvalidateAll` and `Close` drop every key, and a pool
  closed this way keeps working (the next `Ensure` assembles again).
- **One deferred replacement per key.** A reload that lands on a stale
  but still-serving member hands it off with
  `ScheduleReplacement`: the watcher waits for the drain, asks `Wanted`
  (a key the user left is not rebuilt), ensures, and announces the
  member through `Replaced`. A second arm inside the same drain is
  refused, and a drain that already finished between the arm and the
  watcher still gets its replacement. A drain includes a member a caller
  holds: it finishes at that caller's `Release`, so a caller arming a
  replacement must not wait for `Replaced` before letting go of the
  member it is holding.
- **`Do` absorbs the lifecycle guards.** `Do(ctx, key, stop, fn)`
  resolves the member and runs `fn` on it, retrying the errors
  `Spec.Retryable` classifies (plus the pool's own `ErrNotReady`) inside
  one window (`RetryWindow`, default 10s) and one attempt budget
  (`RetryAttempts`, default 3). The budget is the whole call's, measured
  by the remaining time per attempt; `stop` is asked before every retry
  and never before the first attempt; a non-retryable error, the
  caller's cancellation and a member resolved as the window closed all
  come back as they are.
- **Configure-once.** `Installed` runs once per member, on every path
  that hands one out, so event wiring is applied exactly once however
  often a member is handed out — and a member the pool already forgot is
  never wired late.
- **What is not here.** Which keys exist and what they mean, what
  triggers an invalidation, the fallback backend an assembly reads, and
  what a member's busy state is: the application's. The pool also does
  not run a member's own cancellation or drain — `Close` and
  `WaitClosed` are hooks into the application's teardown.

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
  for "hello"`. A package that is not a single plugin is `plugin
  install: zip has no plugin.json` (or `zip holds 2 plugins`), a name
  that leaves the extraction tree is `plugin install: zip entry "../x"
  escapes the archive`, and an archive past the caps is `plugin
  install: zip entry "huge.bin" unpacks to N bytes, over the N-byte
  limit`.
- Uninstall and update: removing a builtin is `Forbidden` (`plugin
  uninstall: hello is a builtin plugin; disable it instead`); a remote
  update that fails its checks is `plugin update: checksum mismatch
  (want sha256:..., got sha256:...)`, `plugin update: package holds
  plugin "x", wanted "y"` or `plugin update: plugin "x" declares no
  update.url`. See the `flowcraft-config` reference card for the full
  error → cause table.

## Status

Craft lives in the workspace and is **not yet released**: no `craft/vX.Y.Z`
tag exists, and `craft.Version` stays `"0.0.0"` for development builds. The
host primitive set is **v1** and its tool names are a wire contract. The
plugin install / update / rollback / uninstall *mechanics* are craft's
(`Store.Install`, `InstallZip`, `Rollback`, `Uninstall`, and the
`plugin.Installer` seam an application wires its transport into); the
surfaces around them — update feeds and channels, UI orchestration, the
transport itself, and the desktop / shell side — are not; the app owns
them. Do not assume behaviour beyond what the code and the reference
cards describe.

The dynamic agent coordinator (`Craft.SyncAgents`) is craft's; the
declaration store behind it — file schema, versioning, CRUD tools — is
the application's.

The pool mechanism (`craft/pool`) is craft's; the keys, the triggers
that invalidate them, and the wiring of one member — a Craft runtime,
mostly — are the application's.

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
