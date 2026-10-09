# Craft definition and plugin schema

Owned by `github.com/GizClaw/flowcraft/craft` (module `craft`). The
validator does **not** cover craft.yaml: no `craft/vX.Y.Z` release exists
yet, so a standalone validator cannot resolve the module. A craft
definition is checked by the host with `craft.ParseDefinition`, and its
factories by the host registry (see "What the host must still provide").

## craft.yaml card

Decoded by `craft.ParseDefinition` (YAML or JSON) and validated by
`Definition.Validate`. Unknown fields are rejected at decode.

```yaml
craft:
  id: <string>              # required
  name: <string>            # optional
  version: <version>        # required, dotted numeric
  min_host_version: <version> # optional compatibility floor
ui:                         # optional; host app frontend, not plugins
  assets: <path>            # optional
  entry: <path>             # optional
  kinds: [<string>]         # optional
plugins:                    # optional
  builtin: <dir>            # optional, read-only plugin dir
  roots: [<dir>]            # optional, scanned plugin dirs
  tool_registry: <resource> # optional, resource that gets plugin tools
  node_targets: [<agent>]   # optional, agents that get plugin nodes
host_tools:                 # optional
  disable: [<primitive>]    # optional, primitives never exposed
  services:                 # optional, named impl per family
    <family>: {impl: <name>}
deploy: <deploy document>   # inline; XOR base_layers
base_layers:                # XOR deploy
  - name: <string>          # optional, defaults base-<i>
    priority: <int>         # optional
    file: <path>            # exactly one of file|embed
    embed: <name>
```

Invariants that fail validation (`errdefs.Validation`):

- `craft.id` required; `craft.version` required.
- `deploy` and `base_layers` are mutually exclusive and one is required.
- A `base_layers` entry needs exactly one of `file` or `embed`.
- `min_host_version` must be a valid version; a release host older than
  the floor is rejected (`Version != "0.0.0"` guard).

Notes:

- `plugins.builtin` and `plugins.roots` are declarative only; the `craft`
  package reads only `tool_registry` and `node_targets`. The host
  configures `plugin.Store` roots from `builtin`/`roots`.
- `ui` describes the host application's own frontend. It is not the
  plugin UI channel.
- `host_tools.services.<family>.impl` must name an impl registered by a
  capability; otherwise `craft.New` fails. The families are `secrets`,
  `workspace`, `browser`, `inference`, `sessions`, `telemetry`, `events`.

## plugin.json manifest card

Decoded by `plugin.ParseManifest`, validated by `Manifest.Validate(root)`
(root empty skips filesystem checks). Strict decode; ≤ 1 MiB.

```json
{
  "id": "<string>",
  "name": "<string>",
  "version": "<version>",
  "minHostVersion": "<version>",
  "permissions": ["<permission>"],
  "ui": {"entry": "<path>"},
  "mcp": {
    "name": "<string>", "transport": "stdio|http",
    "command": "<cmd>", "args": ["<arg>"],
    "env": {"<key>": "<value>"},
    "url": "<url>", "headers": {"<k>": "<v>"},
    "required": false
  },
  "skills": ["<path>"],
  "hooks": ["<path>"],
  "nodes": [{"type": "<t>", "tool": "<tool>", "desc": "<s>", "timeout": "30s"}],
  "update": {"url": "<url>"}
}
```

- `id`: `^[a-z0-9][a-z0-9._-]{0,63}$`. `version` required; `version`
  and `minHostVersion` are dotted numeric with optional prerelease. Leave
  `minHostVersion` out against a development build of craft
  (`Version == "0.0.0"`), where any floor above `0.0.0` makes the plugin
  invalid; set it once you pin a released craft.
- Legacy aliases: `entry` (⇒ `ui.entry`), `mcpServers` (⇒ `mcp`; the two
  are mutually exclusive). `kraft` and `tools` are rejected with an
  upgrade hint.
- Limits: skills ≤ 32, hooks ≤ 16, MCP servers ≤ 16, nodes ≤ 64;
  `mcp.env` ≤ 32 entries, key ≤ 128, value ≤ 4 KiB; paths ≤ 256 and
  confined to the plugin root; UI asset ≤ 10 MiB.
- `mcp.transport`: `stdio` (default; needs `command`) or `http` (needs
  `url`). `command`/`args` carrying a path separator are resolved against
  the plugin root; bare names go through `PATH`. `mcp` and `mcpServers`
  in the same manifest is rejected.
- Tool names are namespaced per server: one server publishes under the
  plugin's namespace, several publish under the plugin's plus the
  server's `name` (or its position, when unnamed). Two of a plugin's own
  servers collapsing into one namespace is rejected, and so is a plugin
  whose namespace an installed plugin already holds — the second one is
  listed with an error instead of silently losing every tool it
  publishes.
- `update.url`: optional, ≤ 2048 bytes, absolute `http(s)` URL without
  credentials or fragment. craft validates the shape and hands the URL to
  a `plugin.Installer`; it never fetches anything itself.

### Permissions

| Permission | Gates |
| --- | --- |
| `mcp:provide` | `mcp` server section |
| `skills:provide` | `skills` list |
| `hooks:provide` | `hooks` list |
| `nodes:provide` | `nodes` section |
| `ui:webview` | UI bundle (shell gate; craft serves the bundle and lists the grant in `ui.Registry` entries) |
| `storage:kv` | plugin KV (shell binds; `Host.KV` is the host accessor) |
| `secrets:auth` | `secret_get` / `secret_set` / `secret_delete` |
| `inference:write` | `inference_upsert` / `inference_remove` |
| `telemetry:export` | `telemetry_configure` / `telemetry_disable` |
| `sessions:import` | `session_import` / `session_imported_sources` |
| `host:open_url` | `open_url` |
| `events:emit` | `emit_event` |

An unknown permission is rejected (`plugin <id>: unknown permission`).

### Drop rules (fail closed)

A gated section declared without its permission is dropped; the plugin
still loads and enables.

- `mcp` without `mcp:provide`: no child process, no token, no tools; a
  node bound to it is `Forbidden` with the missing grant.
- `skills` without `skills:provide`: `SkillRoots` omits the plugin.
- `hooks` without `hooks:provide`: `HookSources` omits the plugin.
- `nodes` without `nodes:provide`: no node resource, no engine dep.
- Disabling a plugin withdraws its contributions even with permissions
  intact.

### UI delivery

`Craft.UI()` (`craft/ui.Registry`, nil without a plugin host) is the
whole UI surface: `Entries()` → id / name / version / entry /
permissions / enabled + revision, `Assets(id)` → an `fs.FS` rooted at
the directory holding `ui.entry` (confined, ≤ 10 MiB per file), and
`craft.ui.changed` (`{revision}`, same counter as the plugin store) on
every enable / disable / install / update / rollback. The shell reloads
every bundle on each event: destroy the old plugin scopes, re-read
`Entries()`, load again. Runtime registration, rendering and the
`ui:webview` gate are the shell's, not craft's.

### Install / update / uninstall

Directory and zip installs share one pipeline
(`Store.Install(ctx, dir)`, `Store.InstallZip(ctx, zipPath)`):
`Inspect` validation, a **strictly newer** version over an installed
plugin, a snapshot to `<root>/.backups/<id>` before the replace, restore
on failure, nothing left behind by a failed fresh install. The install
lands in the last writable root, the one the scan prefers. An id that
exists only as a builtin takes a user copy that shadows it (newer than
the builtin, and never older than a builtin the application has since
updated); `Store.Rollback(ctx, id)` restores the snapshot.

A zip package holds the files at the archive root or under one top-level
directory. Refused: absolute names, `..` elements, backslashes,
symlinks, anything not a file or a directory, one entry unpacking past
`MaxZipEntryBytes` (64 MiB) or a package past `MaxZipBytes` (256 MiB) —
caps read from the headers, which archive/zip itself enforces.
Executable bits survive; everything else is written 0600/0700.

`Store.Uninstall(ctx, id, UninstallOptions{})` removes the directory,
the snapshot, the enable state and by default the KV file and the data
directory; `KeepKV` / `KeepData` keep them. Builtins are refused
(`Forbidden`). `Host.Uninstall` drains the plugin before removing it.
App state keyed by plugin id (secrets, inference profiles) is not
craft's.

`Store.UpdateFrom(ctx, id, installer)` runs a remote update: the
manifest's `update.url` through `Installer.Check`, validate version /
download URL / `sha256:<64hex>` checksum, require a newer version,
`Installer.Fetch`, verify the checksum (≤ 256 MiB), require the package
to hold the same plugin at the announced version, then install.
Transport policy (https-only, proxies, credentials, SSRF) is the
application's, behind the `Installer` seam.

### Directory layout the host scans

```text
<root>/<id>/plugin.json     # required
<root>/<id>/dist/...        # ui.entry target (optional)
<root>/<id>/server/...      # MCP server files (optional, any language)
<root>/<id>/skills/...      # optional
<root>/<id>/hooks/...       # optional
```

Directories starting with `.` or `_` are skipped. Later roots shadow
earlier ones with the same id; user roots shadow builtin ids
(`ShadowsBuiltin` + recorded builtin version).

## host_tools service bindings and disable list

```yaml
host_tools:
  disable: [open_url]
  services:
    secrets:   {impl: keyring}
    workspace: {impl: dynamic}
    browser:   {impl: desktop}
    inference: {impl: app}
    sessions:  {impl: app}
    telemetry: {impl: otlp}
    events:    {impl: app}
```

Each `impl` names a registration from a capability's
`RegisterServices(*hostmcp.ServiceRegistry)`, keyed by `family.name`. The
family's service interface is fixed:

| Family | Interface | Primitives |
| --- | --- | --- |
| `secrets` | `SecretService` | `secret_get`, `secret_set`, `secret_delete` |
| `workspace` | `ContextService` | `workspace_current` |
| `browser` | `BrowserService` | `open_url` |
| `inference` | `InferenceService` | `inference_upsert`, `inference_remove` |
| `sessions` | `SessionService` | `session_import`, `session_imported_sources` |
| `telemetry` | `TelemetryService` | `telemetry_configure`, `telemetry_disable` |
| `events` | `EventService` | `emit_event` |

A nil service removes its primitives from the exposed set (fail closed);
`disable` reports them as `disabled` in `host_about`.

## Error → cause

| Error text | Cause |
| --- | --- |
| `craft: definition id is required` | empty `craft.id` |
| `craft: definition version is required` | empty `craft.version` |
| `craft: deploy and base_layers are mutually exclusive` | both set |
| `craft: definition requires deploy or base_layers` | neither set |
| `craft: base_layers[0] requires file or embed` | entry has neither |
| `craft: base_layers[0] declares both file and embed` | entry has both |
| `craft: requires host version >= X, running Y` | release host below floor |
| `craft: duplicate capability "x"` | two capabilities share a `Name()` |
| `craft: capability app register: ...` | `Registrar.Register` failed |
| `craft: host_tools.services.secrets impl "x" is not registered` | no `ServiceRegistrar` registered `secrets.x` |
| `craft: host_tools.services has unknown family "x"` | typo in the family key |
| `craft: service secrets.x is *T, want SecretService` | impl type mismatch |
| `craft: not started` (`ErrNotStarted`) | `OpenRuntime` before `Start` |
| `craft: runtime already exists` (`ErrRuntimeExists`) | duplicate runtime key |
| `craft: runtime not found` (`ErrRuntimeNotFound`) | unknown key |
| `craft: runtime key is required` | empty key |
| `craft: plugins.tool_registry resource "X" not found` | mount point missing |
| `craft: plugins.node_targets agent "X" not found` | target agent missing |
| `craft: ${craft:} requires a name` | `${craft:}` with no name |
| `craft: reference ${craft:NAME} is not defined` | unknown value/root |
| `plugin: invalid id "X"` | id outside the id pattern |
| `plugin <id>: invalid version "X"` | bad `version`/`minHostVersion` |
| `plugin <id>: unknown permission "x"` | permission not in the table |
| `plugin <id>: at most N skills\|hooks\|MCP servers\|graph nodes` | over limit |
| `plugin <id>: mcp server 0 requires command` | stdio without `command` |
| `plugin <id>: mcp server 0 requires url` | http without `url` |
| `plugin <id>: mcp and mcpServers are mutually exclusive` | both set |
| `plugin <id>: the kraft field is no longer supported; ...` | removed field |
| `plugin: path "X" escapes the plugin root` | path outside the root |
| `plugin <id>: requires host version >= X` | scan gate (entry error) |
| `plugin <id>: mcp servers 0 and 1 share the tool namespace "X"` | two of the plugin's own servers collide |
| `plugin <id>: the tool namespace "X" collides with plugin "Y"` | two installed plugins would publish the same tool names |
| `plugin store: read\|parse <path>` | unreadable `enabled.json`; every plugin reads as disabled and the next write keeps them that way |
| `plugin install: <id> version V is not newer than W` | downgrade/equal |
| `plugin install: no rollback snapshot for "id"` | nothing to roll back |
| `plugin install: zip has no plugin.json` | package is not a plugin |
| `plugin install: zip holds N plugins (...)` | more than one manifest |
| `plugin install: zip entry "X" escapes the archive` | absolute, `..` or backslash name |
| `plugin install: zip entry "X" is a symlink` | symlink or non-regular entry |
| `plugin install: zip entry "X" unpacks to N bytes, over the ... limit` | entry or package over the caps |
| `plugin uninstall: X is a builtin plugin; disable it instead` (`Forbidden`) | builtin removal |
| `plugin uninstall: plugin "X" not found` (`NotFound`) | unknown id |
| `plugin update: plugin "X" declares no update.url` (`NotFound`) | no source declared |
| `plugin update: checksum mismatch (want sha256:..., got sha256:...)` | package did not match its digest |
| `plugin update: package holds plugin "X", wanted "Y"` | wrong plugin in the package |
| `plugin update: package version V does not match the announced W` | stale package under a new version |
| `plugin update: <id> V adds permissions p; the installer does not approve update grants — ...` (`Conflict`) | update widens the grants and the `Installer` is not an `UpdateApprover` |
| `plugin update: <id> V: approve grants: ...` | an `UpdateApprover` refused the expansion |
| `plugin update: Installer is required` | `UpdateFrom` with no installer |
| `plugin <id>: the mcp section is ignored without the mcp:provide permission` | node/direct call to a dropped `mcp` section |
| `hostmcp: tool "x" already registered` | duplicate primitive name |
| `craft manager: craft.yaml not found` | definition not located |
| `craft manager: another instance holds <path>` | lock held |
| `craft manager: Lock requires Paths.DataDir` | lock without data dir |
| `craft manager: Replace requires a running Craft` | replace while stopped |
| `craft manager group: max instances reached (N)` | `MaxInstances` hit |

## What the host must still provide

Craft selects and configures; the host supplies everything executable:

- **Resource factories** for every kind the deploy document references,
  via `Capability.Register`. Craft registers only the plugin node factory
  (`graph.NodeType`/`mcp`) itself.
- **Service implementations** for the host primitives, via
  `Options.HostServices` (direct or as a `HostServices` capability) or
  named impls + `host_tools.services`.
- **UI rendering**: craft delivers plugin bundles (`Craft.UI()` registry,
  `Assets`, `craft.ui.changed`) plus the `ui` config; the shell loads
  and draws them, and gates `ui:webview` / `storage:kv`.
- **Credentials**: secrets, provider keys and any `${env:...}` values.
- **Plugin roots**: the host builds `plugin.Store` and `plugin.Host`
  (`Roots`, `StateDir`, `DataDirRoot`, `HostVersion`) and passes the host
  as `Options.Plugins`.
- **Update transport**: the `plugin.Installer` an update path is wired
  into (`Check` / `Fetch` — https-only, proxies, credentials, SSRF
  policy), plus the surfaces around installing: feeds, channels, the
  install dialog.
- **Process lifecycle** (optional): `craft/manager` or your own shell.

See [craft.md](../../../docs/guides/craft.md) for the full guide.
