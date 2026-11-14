# Runtime layer

`runtime.layers[].path` names a YAML file: the substrate one agent runs on.
Layers merge by `priority` (higher wins) — mappings key by key, **sequences
replaced whole**, scalars replaced — and placeholders resolve once, after the
merge.

```yaml
version: 1
name: assistant

files:
  roots: [/workspace/assistant]      # absolute, at least one; also the write policy

sandbox:
  backend: seatbelt                  # local | seatbelt | bwrap | windows | remote
  workdir: /workspace/assistant      # default: the first files root
  writes: [/workspace/assistant/out]
  net:
    mode: allow_list                 # default | none | allow_list | proxy
    allow_hosts: [api.openai.com, "*.github.com"]
  timeout: 60s
  output_mib: 8

inference:
  providers:
    - name: luna                     # deployment identity, referenced by models
      driver: openai                 # a name the host compiled in
      settings: {api_key: ${OPENAI_API_KEY}}
  models:
    - provider: luna                 # position = priority
      model: gpt-5
      limits: {max_input_tokens: 200000, max_output_tokens: 32000}
  route:
    attempts_per_target: 2
    max_targets: 3

tools:
  enable: [files]                    # groups: files, exec, skills; empty = all
  dynamic:
    default: direct                  # always | direct | deferred | hidden
    always: [tool_search]
  images: false
  middleware:
    - {name: result_limit, settings: {max_bytes: 20000}}

skills:
  roots: [.agents/skills]
```

## `sandbox`

| Field | Rule |
| --- | --- |
| `backend` | Required, no default. |
| `workdir` | Where commands run; empty means the first files root. |
| `writes` | Absolute paths a command may write; empty means read-only. |
| `net.mode` | `default`, `none`, `allow_list`, `proxy`. |
| `net.allow_hosts` | Only with `allow_list`. |
| `net.proxy` | Only with `proxy`. |
| `timeout` | Default per-call deadline; a tool may ask for less. |
| `cpu_millicores` | Needs `timeout` (the budget is derived from it). |
| `memory_mib`, `output_mib` | Caps; zero means unbounded / backend default. |
| `journal` | A settings block; presence opts in. |
| `bwrap`, `seatbelt`, `windows` | That backend's own construction options — on any other backend it is an error. |
| `remote` | Client configuration for `backend: remote`, absent otherwise. |

The budget reaches every call: `timeout`, `cpu_millicores`, `memory_mib` and
`writes` are projected onto each command, merged per field and taking the stricter
side. A cap the backend cannot enforce (a cpu budget on `local`, `writes` on
`local` or `remote`) is a build error or a warning — never silence.

### A backend's own block

```yaml
sandbox:
  backend: bwrap            # or seatbelt
  bwrap:
    binary: /usr/bin/bwrap  # resolved against the runner root
    extra_flags: [--die-with-parent]   # bwrap only; policy-weakening flags are refused
    readonly_root: true
```

```yaml
sandbox:
  backend: windows
  windows:
    write_confine: true     # Low-integrity token; without it the runner is lifecycle-only
```

### Journal

```yaml
sandbox:
  journal:
    exclude: [.git, node_modules]   # never watched (the cost control)
    retention: 4096                 # events kept for replay (default 4096)
    max_watch_set: 20000            # watch budget; past it, a capacity gap
    ops: [create, rename, remove]   # drop "write" events
```

A platform without a watch source fails the build when a journal is configured.

### Remote

```yaml
sandbox:
  backend: remote
  remote:
    transport: unix        # http (default) | unix
    socket: /run/flowcraft/sandbox.sock
    timeout: 30s
    poll_hold: 15s
```

`transport: http` needs `endpoint` (`http(s)://…`) and rejects `socket`; `unix`
needs `socket` and rejects `endpoint` and `insecure_skip_verify`. Files do not
cross the wire: the file tools need that host's filesystem mounted where they run,
which is why `tools.files` with `backend: remote` warns.

## `files`, `skills`, `tools`

| Section | Rule |
| --- | --- |
| `files.roots` | At least one, all absolute; also the write policy the file tools enforce. |
| `skills.roots` | Directories scanned for `SKILL.md`; one index, shared by the tools and the host. |
| `tools.enable` | Tool **groups** (`files`, `exec`, `skills`), not individual tool names; empty enables every registered group. |
| `tools.dynamic` | The projection: `default`, `always`, `hidden`, `budget`, `max_bytes`, `idle_rounds`, `search_with_load`. |
| `tools.images` | Publish the image-reading branch of the file tools. |
| `tools.mcp` | `defer` plus `servers[]`: `transport: stdio|http`, `command`/`url`, `prefix`, `required`, `liveness`. |
| `tools.middleware` | Ordered list of `{name, settings}`; built-ins: `telemetry`, `recover`, `result_limit`, `concurrency`, `redact`. `recover` cannot be disabled. |

## `inference`

Providers are `{name, driver, settings}` — `settings` is the driver's own shape,
decoded by the host's adapter, not by the document. Models are
`{provider, model, weight, capabilities, limits}`; position is priority. `route`
takes `selector`, `selector_settings`, `attempts_per_target`, `max_targets`,
`base_delay`, `max_delay`.

A name the host did not register fails the build with the offered list:
`inference.providers[luna].driver: "openai" is not available; offered: openai`.
