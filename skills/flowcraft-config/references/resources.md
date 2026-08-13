# Resource sub-documents

Sub-documents are attached through `settings: {file: ...}` or inline content.
Each factory owns its settings schema.

## inference provider

```yaml
id: deepseek
spec:
  api: chat
profiles:
  - secrets:
      api_key: {resolver: env, key: DEEPSEEK_API_KEY}
```

Provider IDs and profile IDs must be identifiers. Secret values are resolved
through the host's resolver registry. Provider drivers are registered from
`driver/<name>`.

## inference assembly

The assembly consumes provider resources through `deps`:

```yaml
infer:
  kind: inference.Assembly
  impl: unified
  deps:
    provider: provider
```

Routing settings are factory-owned; optional route policy is configured in the
assembly settings.

## workspace

```yaml
root: ./workspace
scoped:
  enabled: true
  deny_read: ["**/.env"]
  allow_write: ["**"]
  mandatory_deny: [".git/**"]
```

Relative roots resolve against the deployment loader's base directory.

## sandbox

```yaml
root: ./sandbox
```

The local runner accepts a root. Platform backends (`bwrap`, `seatbelt`) are
registered from `core/sandbox/{bwrap,seatbelt}`.

## tool source / assembly

```yaml
sim:
  kind: tool.Source
  impl: sim
tools:
  kind: tool.Assembly
  impl: memory
  deps:
    tool: sim
  settings:
    dynamic:
      default: deferred
      exposures:
        tool_search: always
```

## memory

Memory implementations are app-registered. `core/memory` supplies contracts
and hooks; each implementation owns its settings document.

## graph

Graph definitions are engine settings:

```yaml
engine:
  kind: agent.Engine
  impl: graph
  settings:
    graph: {file: ./graphs/assistant.yaml}
    script_runtime_name: js
```

See [graph.md](graph.md) for the JSON schema.
