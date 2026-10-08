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
  "nodes": [{"type": "<t>", "tool": "<tool>", "desc": "<s>", "timeout": "30s"}]
}
```

- `id`: `^[a-z0-9][a-z0-9._-]{0,63}$`. `version` required; `version`
  and `minHostVersion` are dotted numeric with optional prerelease.
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

### Permissions

| Permission | Gates |
| --- | --- |
| `mcp:provide` | `mcp` server section |
| `skills:provide` | `skills` list |
| `hooks:provide` | `hooks` list |
| `nodes:provide` | `nodes` section |
| `ui:webview` | UI bundle (shell gate; craft only validates/publishes) |
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
- `hooks` without `hooks:provide`: `HookFiles` omits the plugin.
- `nodes` without `nodes:provide`: no node resource, no engine dep.
- Disabling a plugin withdraws its contributions even with permissions
  intact.

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
| `plugin install: <id> version V is not newer than W` | downgrade/equal |
| `plugin install: no rollback snapshot for "id"` | nothing to roll back |
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
- **UI rendering**: craft delivers `ui` and plugin bundles; the shell
  loads and draws them, and gates `ui:webview` / `storage:kv`.
- **Credentials**: secrets, provider keys and any `${env:...}` values.
- **Plugin roots**: the host builds `plugin.Store` and `plugin.Host`
  (`Roots`, `StateDir`, `DataDirRoot`, `HostVersion`) and passes the host
  as `Options.Plugins`.
- **Process lifecycle** (optional): `craft/manager` or your own shell.

See [craft.md](../../../docs/guides/craft.md) for the full guide.
