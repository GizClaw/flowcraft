# Anvil — a minimal craft host application

Anvil is the runnable example for the `craft` module. It is one small host
application that assembles a Craft out of a `craft.yaml` definition, one
compile-time capability and one plugin directory, then walks the parts of the
craft lifecycle a real shell (desktop, HTTP, headless) drives: scanning
plugins, starting the host, opening keyed runtimes, calling tools, reloading a
runtime, hot-plugging a plugin, shutting down.

It needs no model provider, no network access and no credentials — the whole
tour is local and deterministic, so it doubles as an integration test.

中文版见 [README_zh.md](README_zh.md)。

## Quickstart

Prerequisite: Go 1.26+. The plugin's MCP server is a Go program that the
manifest starts with `go run`, so the **first** run compiles it (a few
seconds, then it is cached).

```bash
cd examples/anvil
go run .
```

From the repository root:

```bash
go run ./examples/anvil -definition examples/anvil/craft.yaml -data examples/anvil/.anvil
```

Flags:

- `-definition <path>` — the craft definition (default `craft.yaml`).
- `-data <dir>` — writable root for plugin state, plugin data, the notes files
  and the per-runtime layer the tour writes (default `.anvil/`, gitignored).
- `-timeout <duration>` — bound for the whole tour (default `3m`).

The tour resets the files it owns under the data directory on every run, so it
is repeatable; everything else in there is left alone.

## What the tour shows

| Step | What happens | The craft concept |
| --- | --- | --- |
| definition | `craft.yaml` is parsed and validated | definition vs. application code |
| plugins | the plugin directory is scanned, permissions and skill roots are listed | the plugin store; `skills:provide` |
| start | the Craft starts, the plugin's MCP server comes up, its tools are published | the plugin host and the shared tool set |
| `runtime "default"` | one runtime opens with a value; tools are called through its assembly | keyed runtimes, `${craft:...}`, tool assembly |
| `runtime "alpha"` | a second runtime opens from a per-runtime layer | per-runtime layers, runtime isolation |
| reload `"alpha"` | the layer file is rewritten and the runtime reloaded | transactional reload |
| hot plug | the plugin is disabled and enabled again | plugin contributions are withdrawn and restored |
| shutdown | the runtime and the Craft close | drain-then-close, reverse order |

The interesting excerpts:

```text
== runtime "default"
   tools: hello__greet, hello__ping_host, notes_add, notes_list
   notes_add  "Append one line to the default runtime's notes file."
   event  craft.runtime.opened  {"key":"default"}
   notes_add {"text":"buy milk"} -> added note 1 to /data/notes-default.txt
   notes_list {} -> 1 note in /data/notes-default.txt: buy milk
   hello__greet {"name":"anvil"} -> Hello, anvil! (from the hello plugin)
   event  plugin.hello.ping  {"from":"hello"}
   hello__ping_host {} -> host protocol 1 (host version 0.0.0): 5 primitives exposed, 2 callable by this plugin [emit_event, host_about]; needs a grant for [secret_delete, secret_get, secret_set]; 8 unconfigured (no_service)
```

(Absolute paths are elided to `/data/` here and below.)

Three things happened in that block:

1. `notes_add` and `notes_list` are tools the **capability** registered as the
   resource kind `workshop.Notes`; they act on the file the runtime's
   `${craft:notes}` value points at.
2. `hello__greet` is a tool the **plugin** contributes: its own MCP server
   declared `greet`, and craft prefixed it with the plugin id.
3. `hello__ping_host` is the plugin calling **host primitives** back over the
   host MCP endpoint (`host_about` and `emit_event`); the event it emits
   arrives on the craft plane as `plugin.hello.ping`, and `host_about` reports
   which primitives the host exposes, which ones this plugin may call, and why
   the rest are out of reach (`no_grant`, `no_service`, `disabled`).

```text
== runtime "alpha"
   layer /data/alpha.layer.json: notes.path -> notes-alpha.txt
   notes_add  "Append one line to the alpha runtime's notes file."
   notes_list {} -> no notes yet in /data/notes-alpha.txt
```

Same definition, second runtime key: its own tools, its own notes file. The
default runtime's notes come from a runtime **value**, alpha's from a runtime
**layer** the application writes — both end up in the same composed document.

```text
== reload "alpha"
   rewrote the layer: notes.path -> notes-alpha-round2.txt
   event  craft.reload.started  {"key":"alpha","reason":"manual"}
   event  craft.reload.completed  {"key":"alpha","reason":"manual"}
   notes_list {} -> no notes yet in /data/notes-alpha-round2.txt

== hot plug: disable and enable the hello plugin
   event  craft.reload.completed  {"key":"default","reason":"plugin"}
   tools: notes_add, notes_list
   disabled: the plugin's tools are withdrawn from every open runtime
   tools: hello__greet, hello__ping_host, notes_add, notes_list
   hello__greet {"name":"the restarted plugin"} -> Hello, the restarted plugin!
```

A reload rebuilds the deployment generation and swaps it in atomically; a
plugin change only touches the deployment document and the shared tool set, so
hot-plugging never tears the resource registry. Both surface as
`craft.reload.*` events on the same plane an application already listens to.

## The files

| Path | Role |
| --- | --- |
| `craft.yaml` | The definition: metadata, plugin roots, `host_tools` bindings, one inline `deploy` document |
| `workshop.go` | The one compile-time capability: the `workshop.Notes` resource kind (a tool source), the events and secrets service implementations |
| `assemble.go` | The host wiring: paths → plugin store → plugin host → `craft.New` → event sink; plus the tour's helpers |
| `tour.go` | The scripted lifecycle |
| `events.go`, `print.go` | The craft-plane sink and the mutex-guarded printer that keeps its output readable |
| `tour_test.go` | The same tour as a hermetic test (`go test ./…`) |
| `plugins/hello/plugin.json` | The manifest: permissions, one skill directory, one MCP server |
| `plugins/hello/server/main.go` | The plugin's stdio MCP server (go-sdk) and its host-primitive client |
| `plugins/hello/skills/hello/SKILL.md` | The skill the host exposes through `SkillRoots()` |

## What to copy into your own application

1. **The definition is data; capabilities are code.** `craft.yaml` can only
   name a resource kind, a built-in, or a service implementation an
   already-registered factory provides — `craft.New` fails otherwise, and the
   registry is frozen afterwards. `anvil` registers exactly three factory sets
   (`event.Bus`, `tool.Assembly`, `workshop.Notes`) and two named service
   implementations (`events`, `secrets`).
2. **Paths are the application's.** The definition declares
   `plugins.roots: [plugins]`; `assemble.go` resolves it against the directory
   holding `craft.yaml` and decides where plugin state lives (anvil keeps it
   under the data directory, not next to the definition).
3. **Values and layers are the two per-runtime knobs.** Values feed
   `${craft:...}` (the default runtime's notes file); layers override whole
   subtrees for one key (alpha's). A missing value fails the build with
   `craft: reference ${craft:notes} is not defined`.
4. **Plugin tools arrive asynchronously.** `craft.Start` returns before the
   plugin's MCP server has answered its handshake, so an application waits for
   readiness (`waitForPluginTool`) before it builds a runtime that should carry
   those tools; a live runtime picks up later changes through the shared tool
   set.
5. **Wait on the craft plane, not on sleeps.** Reload effects land on the
   craft's own goroutines; the tour subscribes to `craft.>` (plus the plugin
   namespace) and waits for `craft.reload.completed` when it triggers one.
6. **Read the runtime's resources again after a reload.** A reload swaps in a
   new generation, and `rt.Resource("tools")` returns the new assembly.
7. **A plugin server exits 0 when the host closes its stdin.** That is the
   stdio shutdown the MCP spec prescribes; the plugin host reports a non-zero
   exit as a failed stop. `plugins/hello/server/main.go` shows the check.

## The plugin

`plugins/hello` is a complete plugin: a manifest, a skill and an MCP server.

- `mcp: {command: "go", args: ["./server"]}` — a path-bearing argument is
  resolved against the plugin root and confined to it, so the manifest ships
  source and the host compiles it on start. A real plugin ships a compiled
  binary (or an interpreter plus a script, e.g. `python3 server/main.py`).
- `permissions: [mcp:provide, skills:provide, events:emit]` — each gated
  contribution is dropped fail-closed without its permission, and the
  permission set is what scopes the bearer token the plugin receives.
- The host injects `CRAFT_PLUGIN_ID`, `CRAFT_PLUGIN_DATA_DIR`,
  `CRAFT_HOST_MCP_URL` and `CRAFT_PLUGIN_TOKEN` into the child. The token is
  minted per plugin from its declared permissions; that is why `emit_event`
  works for this plugin and `secret_get` would not.
- `minHostVersion` is deliberately absent: against a development build of
  craft (`Version == "0.0.0"`) any floor above `0.0.0` marks the plugin
  invalid, which is the wrong lesson for an example. Set it once you pin a
  released craft.

## Pre-release notes

`craft` has no release tag yet, so `go.mod` resolves it from the sibling
checkout:

```go
replace github.com/GizClaw/flowcraft/craft => ../../craft
```

When craft is tagged for the first time, drop the `replace`, pin the released
version and the example builds standalone with `GOWORK=off` like
`examples/parallel` does. Until then, run the example from inside the
repository: the plugin's `go run` needs the workspace to resolve craft.

## Extending it

- **Add an agent turn**: drop in the deploy resources from the
  `flowcraft-config` skill's `assets/minimal-craft` (workspace, provider,
  inference assembly, graph agent) and open a session on the runtime.
- **Add a graph node**: declare `nodes` plus `nodes:provide` in the manifest and
  list the agent in `plugins.node_targets`; craft synthesizes one
  `graph.NodeType` resource per node and wires it into the agent's engine deps.
- **Add a capability**: another resource kind, a `${...}` resolver, a layer, or
  a host factory decorator — `craft` discovers the optional interfaces by type
  assertion.
- **Add a second plugin root**: `plugins.roots: [plugins, ~/…/plugins]`, or a
  read-only `plugins.builtin` directory the user roots can shadow.

See [`docs/guides/craft.md`](../../docs/guides/craft.md) for the full contract
and [`skills/flowcraft-config`](../../skills/flowcraft-config/) for the
`craft.yaml` and `plugin.json` reference cards.
