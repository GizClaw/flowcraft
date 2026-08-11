# Resource sub-documents

Each first-party resource wraps its own module document via
`settings: {file: ...}` or inline content. Sub-documents keep their own
version field and strictness.

Every sub-document can be validated standalone:
`validate-config.sh --type inference inference.yaml` (types:
`inference`, `workspace`, `sandbox`, `tool`). The deploy entry validates
them together and additionally checks cross-file wiring.

## inference.yaml

```yaml
version: v1
providers:
  - id: openai             # deployment-chosen ID, identifier pattern
    driver: openai         # selects a registered provider factory
    profiles:
      - secrets:           # default profile when id is omitted ("" = default)
          api_key: {resolver: env, key: OPENAI_API_KEY}
      - id: speech
        operations: [transcription, realtime]
        secrets: {speech_api_key: {resolver: env, key: DOUBAO_SPEECH_API_KEY}}
        spec: {app_id: "123456"}
    spec:
      models: [{name: my-model, kind: generate, reasoning: true}]
route:                    # optional; omit to address exact models yourself
  generate:
    - tier: primary
      targets:
        - model: {id: {provider: openai, name: gpt-5.4}}
  embed:
    - tier: primary
      targets:
        - model: {id: {provider: openai, name: text-embedding-3-small}}
  retry: {...}            # optional
  circuit_breaker: {...}  # optional
```

Rules:
- Secret references only — the envelope validator rejects
  credential-looking keys anywhere in `spec`/`profiles.spec`.
- Provider IDs, driver names, profile IDs, secret names, resolvers must
  match the identifier pattern `[A-Za-z0-9][A-Za-z0-9_-]*`.
- First-party drivers: `openai`, `azure`, `deepseek`, `qwen`,
  `bytedance`, `minimax`, `kimi`, `anthropic`. Each provider package owns
  its `spec` schema and required secret names (usually `api_key`).
- bytedance transcription is session-only (no unary); azure supports
  unary transcription plus image/audio intents; openai realtime is absent.
- A route pool needs at least one target; duplicate tiers/targets and
  unknown models fail the build.

## memory (sdk/memory contracts)

Memory capabilities (`ContextProvider`, `TurnSink`, `DocumentSink`) are
`sdk/memory` contracts; concrete implementations are separate modules and
register their own deploy factories and settings documents. The flowcraft
`memory/` implementation module is not released yet, so no `memory.yaml`
example is included here. The `sdkx/memory/hook` factories
(`memory.context` preparer, `memory.turn` committer) bind a whole
`memory.Assembly` resource as their `memory` dep.

## workspace.yaml

```yaml
version: v1
workspaces:
  project:
    driver: local            # or memory
    settings: {root: workspace}
    scope:
      deny_read: ["**/.env"]   # read defaults to allowed (deny-only)
      allow_write: ["**"]      # write defaults to denied (allow-only)
      mandatory_deny: [".git/**"]
```

Patterns must be relative; absolute, `..`, and malformed patterns are
rejected. Relative `root` resolves against the builder's base dir.

## sandbox.yaml

```yaml
version: v1
sandboxes:
  coding:
    backend: local           # first-party; platform backends register themselves
    workspace: project       # workspace name from the bound workspace registry
    settings: {writable_paths: [".cache"]}
    defaults:
      timeout: 2m
      env: {allow: [PATH, HOME], inject: {X: y}}
      net:
        mode: deny_all       # default | deny_all | allow_list | proxy
      resources: {memory_bytes: 1073741824, max_output_bytes: 1048576}
    allowed_commands: [go, git]
    approval: {outside_workdir: true, sensitive_commands: [rm]}
```

The sandbox resource requires `deps: {workspaces: <workspace resource>}`
(whole workspace registry). Durations are strings.

## tools.yaml

```yaml
version: v1
sources:
  - kind: builtin            # names tools the host registered on the builder
    spec: {tools: [my_tool]}
middlewares:
  - kind: recover            # no spec
  - kind: telemetry          # no spec
  - kind: concurrency
    spec: {limit: 4}
  - kind: timeout
    spec: {default: "30s", per_tool: {slow: "2m"}}
  - kind: ratelimit          # no spec
  - kind: approval
    spec: {tools: [rm]}      # requires host Approver
  - kind: audit              # no spec; requires host AuditSink
  - kind: resultlimit
    spec: {max_chars: 4000, marker: "[truncated]"}
  - kind: redact
    spec: {rules: [{pattern: "(?i)secret", replacement: "***"}]}
scopes:
  my_tool: agent             # agent | platform
```

Unknown source/middleware kinds and unknown builtin tool names fail Build.
`agent.tools` allow-lists are runtime policy gates, not build-time checks.

Sandbox-backed command tools (`exec`, `exec_session`) register on the tool
builder with `exec.RegisterBuiltin(toolBuilder)`. The tool resource then
declares the optional `deps: {sandbox: boxes/<name>}` binding to a
`sandbox.Registry` item; the tools are built from that runner per assembly
and the build fails closed when the dep is missing or not a
`sandbox.Runner`.

## Other first-party settings

- `event.Bus/memory`: `route_cache_size` — 0 disables cache, negative keeps
  default, omitted keeps default.
- `scheduler.Server/local`: no settings allowed.
- `agent.CheckpointStore/sqlite`: `path` required (`:memory:` allowed).
- `delegation.AsyncBackend/kanban-memory`: `scope_id`, `max_pending`,
  `max_cards`, `card_ttl` (duration string); optional `event_bus` dep.
- `agent.ScriptRuntime/js`: `pool_size` (positive), `max_call_stack_size`
  (positive), `max_exec_time` (duration string).
- `agent.ScriptRuntime/lua`: `pool_size` (positive), `max_exec_time`.

## Sources of truth

`docs/guides/inference.md`, `docs/guides/memory.md`,
`docs/guides/workspace.md`, `docs/guides/sandbox.md`,
`docs/guides/tool.md`, and each package's `config/*.go`.
