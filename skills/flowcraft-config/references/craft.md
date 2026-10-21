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
  disable: [resource.op]    # optional, primitives never exposed
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
- `min_host_version` must be a valid version; a host older than the floor
  is rejected. Only a build that resolved no version at all — the
  `"0.0.0"` of a workspace build, a local replacement or an untagged main
  module — skips the comparison. A commit pin resolves the pseudo-version
  go records for it, which is compared like any other.

Notes:

- `plugins.builtin` and `plugins.roots` are declarative only; the `craft`
  package reads only `tool_registry` and `node_targets`. The host
  configures `plugin.Store` roots from `builtin`/`roots`.
- `ui` describes the host application's own frontend. It is not the
  plugin UI channel.
- `host_tools.disable` names primitives the capabilities contribute; an
  entry that names anything else, or names `host.about`, fails
  `craft.New`.

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
  "run": {
    "command": "<cmd>", "args": ["<arg>"],
    "env": {"<key>": "<value>"}
  },
  "skills": ["<path>"],
  "hooks": ["<path>"],
  "nodes": [{"type": "<t>", "tool": "<tool>", "desc": "<s>", "timeout": "30s"}],
  "update": {"url": "<url>"}
}
```

- `id`: `^[a-z0-9][a-z0-9._-]{0,63}$`. `version` required; `version` and
  `minHostVersion` are dotted numeric with an optional prerelease suffix
  of dot-separated letters, digits and dashes — the same validator a
  `craft.yaml` `min_host_version` floor asks. Against a development build
  of craft, whose `Version` resolves to `"0.0.0"`, any `minHostVersion`
  above `0.0.0` makes the plugin invalid: leave it out until the host
  resolves a real version, which is what pinning a released craft does. A
  host pinned to a commit resolves that commit's pseudo-version, so a
  floor above the last release is refused there too, whether it is
  declared by the plugin or by the definition.
- `mcp` and `mcpServers` are rejected with an upgrade hint, and so are
  `kraft` and `tools`: craft serves the plugin socket now, so a plugin
  declares a `run` section and speaks the wire instead of naming a
  server.
- Limits: skills ≤ 32, hooks ≤ 16, nodes ≤ 64;
  `run.env` ≤ 32 entries, key ≤ 128, value ≤ 4 KiB; paths ≤ 256 and
  confined to the plugin root; UI asset ≤ 10 MiB.
- `run.command` is required; `command`/`args` carrying a path separator
  are resolved against the plugin root and must exist there — the same
  resolution the host
  performs when it spawns, so a manifest that installs is one the host
  can start. A path-shaped `command` must be a regular file — exec
  cannot run a directory or anything else — while a path-shaped argument
  only has to exist, because an argument may name a directory on purpose
  (`go run ./server`). Bare names go through `PATH` and are not checked,
  and an argument starting with `-` is never resolved. There is no
  transport field: craft listens on one unix socket per process and
  states it in `FLOWCRAFT_PLUGIN_WIRE` (`FLOWCRAFT_PLUGIN_ID` and
  `FLOWCRAFT_PLUGIN_DATA_DIR` come with it, and the manifest cannot
  override the three).
- Tool names are namespaced per plugin: everything it declares over the
  connection publishes under the plugin's namespace. A plugin whose
  namespace an installed plugin already holds is listed with an error
  instead of silently losing every tool it publishes.
- `update.url`: optional, ≤ 2048 bytes, absolute `http(s)` URL without
  credentials or fragment. craft validates the shape and hands the URL to
  a `plugin.Installer`; it never fetches anything itself.

### Permissions

| Permission | Gates |
| --- | --- |
| `tools:provide` | tools declared over the plugin wire |
| `skills:provide` | `skills` list |
| `hooks:provide` | `hooks` list |
| `nodes:provide` | `nodes` section |
| `ui:webview` | UI bundle (shell gate; craft serves the bundle and lists the grant in `ui.Registry` entries) |
| `storage:kv` | plugin KV (shell binds; `Host.KV` is the host accessor) |

Those six are the ones craft defines. A grant for a host primitive is
declared by the application (`plugin.Options.Grants`), not by this table:
anvil requires `secrets:auth` for `secret.get`/`secret.set`/`secret.delete`
and `events:emit` for `events.emit`, and `pluginserver.Grants(primitives)`
derives the set from a catalog. `Manifest.Validate` accepts both kinds. An
unknown permission is rejected (`plugin <id>: unknown permission`).

### Drop rules (fail closed)

A gated section declared without its permission is dropped; the plugin
still loads and enables.

- `tools` without `tools:provide`: the process still runs, but nothing it
  declares is projected; a direct call is `Forbidden` with the missing
  grant.
- `skills` without `skills:provide`: `SkillRoots` omits the plugin.
- `hooks` without `hooks:provide`: `HookSources` omits the plugin.
- `nodes` without `nodes:provide`: no node resource, no engine dep.
- Disabling a plugin withdraws its contributions even with permissions
  intact.

### Enable waits for the plugin

`Host.SetEnabled(ctx, id, true)` waits for the plugin to say `Ready` on
its connection, bounded by `HostOptions.EnableTimeout`
(`DefaultEnableTimeout`, 10s; a negative value spawns and returns). A
plugin whose command is gone or not executable — files can change under
an installed plugin — whose `Hello` states a major craft cannot speak,
whose identity does not match the connection, or whose `Declare` craft
refuses comes back as that error, with the process stopped and the entry
left disabled (`StopRolledBack`). A process that is merely not there yet
is not a failure: the entry is recorded enabled, and an exit on its own is
reported as `StopFailed`. Enabling a plugin that was reported
`StopFailed` starts it again — the source that gave up is replaced — and
a loss that lands while an enable waits is reported once that call
returns. `Host.Start` replays the enabled set without waiting.

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
<root>/<id>/server/...      # the plugin process's own files (optional, any language)
<root>/<id>/skills/...      # optional
<root>/<id>/hooks/...       # optional
```

Directories starting with `.` or `_` are skipped. Later roots shadow
earlier ones with the same id; user roots shadow builtin ids
(`ShadowsBuiltin` + recorded builtin version).

## host_tools disable list

```yaml
host_tools:
  disable: [secret.get]
```

The primitives a plugin may call back are not configured in the definition:
craft ships none, and a capability declares them in code by implementing
`HostPrimitives() []pluginserver.Primitive`. Each primitive carries its
`resource.op`, its grant name (empty means every plugin may call it), its
description, its params schema and its handler. `host.about` is added by
craft and is the catalog a connection sees.

`disable` therefore only takes primitives away from that contributed set.
It reports them as `disabled` in `host.about`, takes the same
`resource.op` form, and an entry that names a primitive the application
does not contribute (or names `host.about`) fails `craft.New`. Every
primitive is called through the one `Host.Call` RPC with a `resource` and
an `op`.

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
| `plugin host: cannot disable "x": it is not a host primitive` | `host_tools.disable` names a primitive no capability contributes |
| `plugin host: host.about cannot be disabled: it reports the primitive set` | `disable` names `host.about` |
| `plugin host: primitive "x" already registered` | two capabilities contribute the same `resource.op` |
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
| `plugin <id>: at most N skills\|hooks\|graph nodes` | over limit |
| `plugin <id>: run.command is required` | `run` without `command` |
| `plugin <id>: run.command "./x": ...` | path-shaped `command` missing under the plugin root, or not a regular file |
| `plugin <id>: run.arg "./x": ...` | path-shaped argument missing under the plugin root |
| `plugin: the mcp field is no longer supported; ...` | legacy `mcp`/`mcpServers` section |
| `plugin <id>: the kraft field is no longer supported; ...` | removed field |
| `plugin: path "X" escapes the plugin root` | path outside the root |
| `requires host version >= X` | scan gate (`Entry.Error`, no id prefix) |
| `plugin <id>: the tool namespace "X" collides with plugin "Y"` | two installed plugins would publish the same tool names |
| `plugin store: read\|parse <path>` | unreadable `enabled.json`; every plugin reads as disabled and the next write keeps them that way |
| `plugin install: <id> version V is not newer than W` | downgrade/equal |
| `plugin install: no rollback snapshot for "id"` | nothing to roll back |
| `plugin install: zip has no plugin.json` | package is not a plugin |
| `plugin install: zip holds N plugins (...)` | more than one manifest |
| `plugin install: zip entry "X" escapes the archive` | absolute, `..` or backslash name |
| `plugin install: zip entry "X" is a symlink` | symlink entry |
| `plugin install: zip entry "X" is not a regular file` | entry that is neither a file nor a directory |
| `plugin install: zip entry "X" unpacks to N bytes, over the ... limit` | entry or package over the caps |
| `plugin uninstall: X is a builtin plugin; disable it instead` (`Forbidden`) | builtin removal |
| `plugin uninstall: plugin "X" not found` (`NotFound`) | unknown id |
| `plugin update: X declares no update.url` (`NotFound`) | no source declared |
| `plugin update: checksum mismatch (want sha256:..., got sha256:...)` | package did not match its digest |
| `plugin update: package holds plugin "X", wanted "Y"` | wrong plugin in the package |
| `plugin update: package version V does not match the announced W` | stale package under a new version |
| `plugin update: <id> V adds permissions p; the installer does not approve update grants — ...` (`Conflict`) | update widens the grants and the `Installer` is not an `UpdateApprover` |
| `plugin update: <id> V: approve grants: ...` | an `UpdateApprover` refused the expansion |
| `plugin update: Installer is required` | `UpdateFrom` with no installer |
| `plugin <id>: tools are ignored without the tools:provide permission` | direct call to a tool of a plugin without the grant |
| `plugin host: primitive "secret.get" already registered` | two capabilities contribute the same `resource.op` |
| `plugin host: cannot disable "x": it is not a host primitive` | `host_tools.disable` entry names nothing in the catalog |
| `plugin host: host.about cannot be disabled: it reports the primitive set` | `host_tools.disable` names `host.about` |
| `craft manager: craft.yaml not found` | definition not located |
| `craft manager: another instance holds <path>` | lock held |
| `craft manager: Lock requires Paths.DataDir or LockPath` | lock with neither a data dir nor an explicit lock path |
| `craft manager: Replace requires a running Craft` | replace while stopped |
| `craft manager group: max instances reached (N)` | `MaxInstances` hit |

## What the host must still provide

Craft selects and configures; the host supplies everything executable:

- **Resource factories** for every kind the deploy document references,
  via `Capability.Register`. Craft registers only the plugin node factory
  (`graph.NodeType`/`plugin`) itself.
- **Host primitives** for the plugin wire, via a capability implementing
  `HostPrimitives() []pluginserver.Primitive`. Craft contributes only
  `host.about`; a host that declares nothing still answers it.
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
