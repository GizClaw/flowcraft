# Deployment document

`deploy.yaml` declares which agents exist and what each is made of. One document,
any number of agents; each agent is an engine plus the substrate that engine runs
on, and optionally an external surface.

```yaml
version: 1
agents:
  - name: assistant
    engine:
      kind: graph                     # required, no default
      settings: {path: ./graphs/assistant.yaml}
    runtime:
      layers:                        # at least one for a local engine
        - {priority: 0, path: ./runtime.yaml}
        - {priority: 10, path: ./local.yaml}
    catalog: {scope: run}            # run (default) | session
    protocol:                        # absent: not exposed
      kind: a2a
      settings: {card: {version: "1.0.0", url: https://assistant.example}}
    delegation:                      # absent: no delegation tools at all
      targets: [reviewer]
      modes: [sync]
    observability: {trace: run}      # off (default) | run | step | node
    hooks:                           # chained in declaration order
      - kind: audit
        settings: {sink: stdout}
```

## Fields

| Field | Rule |
| --- | --- |
| `version` | Must be `1`. |
| `agents[].name` | Required, unique in the document, and what a caller addresses. |
| `agents[].engine` | Required. `kind` names a registered kind; there is no default. |
| `agents[].runtime` | A stack of layers, at least one. Absent is legal only for an engine kind that reports `Local: false`. |
| `agents[].catalog` | `scope: run` (default) or `session`: how far a tool projection is scoped. |
| `agents[].protocol` | Absent means the agent is not exposed. `kind` is required when the block is present. |
| `agents[].delegation` | Absent means **no** delegation tools, not "unrestricted". |
| `agents[].observability` | Absent fields fall back to the host's bindings; "silent" is not "off". |
| `agents[].hooks` | Chained in declaration order, before the host's own hooks. |

Unknown fields, duplicate agent names and unregistered kinds are load errors. A
kind the document names but the host did not register fails with the registered
names in the error — never by being skipped.

## What ships

```text
engines:   a2a (remote client) · graph (core/agent-graph)
protocols: a2a (server)
hooks:     none — a host's policy is the host's
```

A host adds its own with `deploy.BuiltinKinds().WithEngines(...).WithHooks(...)
.WithProtocols(...)`. Registration lives in `deploy`; the implementation lives in
its own package.

## Layers

`runtime.layers` is ordered by `priority` (higher wins), not by list position: a
profile picked on a command line belongs above a file found by a directory scan
whatever order the scan produced. Duplicate priorities are rejected.

Merge rules: mappings merge key by key, **sequences are replaced whole**, scalars
are replaced. A permission list must not grow because somebody added a line.

Placeholders are resolved once, after the merge, so a value an overriding layer
replaced is never looked up:

| Form | Resolves through |
| --- | --- |
| `${NAME}` | the environment |
| `${NAME:-fallback}` | the environment, with a default |
| `${scheme:arg:arg}` | a resolver the host registered |

## Placeholders before start

`deploy.Loader.RequiredPlaceholdersFile` reports every variable the document
names, in one pass — a startup checklist instead of a restart loop. `Load` then
fails at the first unset reference.

```go
requirement, err := loader.RequiredPlaceholdersFile("deploy.yaml")
// requirement.Required   unset and without a fallback: the process cannot start
// requirement.Optional   has a fallback: absent is fine
// requirement.Sources    which value names it, for the error message
```

## What the loader checks

`Loader.Load` does all of this without building anything:

1. expands placeholders on the parsed document;
2. decodes strictly — an unknown field is an error, not a hint;
3. `version` is `1`, at least one agent, names present and unique;
4. each `engine.kind` is registered (error lists what is), and `Decode` runs on
   its `settings` so a typo fails here;
5. the same for `protocol.kind` and `hooks[].kind`;
6. `catalog.scope` is one of the two values, `delegation` modes and bounds are
   valid;
7. each runtime layer is loaded, merged and validated by `runtime.Config.Validate`
   (see [runtime.md](runtime.md));
8. a graph engine's `settings.path` file is read and validated.

It does **not** construct a provider, a sandbox or a tool: settings semantics,
credentials and endpoint reachability are the host's business at start time, and
a graph's node *kinds* are checked when the registry exists.

## Errors

```text
manage: version 2 is not supported; this build reads 1
manage: agents[0]: name is required
manage: agents[0]: duplicate name "forge"
manage: agents[forge].engine: kind "grpah" is not registered; offered: a2a, graph
manage: agents[forge].runtime: at least one layer is required
manage: catalog.scope "per_call" is not supported; use "run" or "session"
```
