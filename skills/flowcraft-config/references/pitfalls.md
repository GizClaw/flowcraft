# Pitfalls and build-failure troubleshooting

## Common pitfalls

The validator (`validate-config.sh`) is structural only: it enforces the
subset marked **validator**. Everything else surfaces only when the host
builds the deployment with its own factory registry, or at runtime.

1. Agent engine deps belong under `engine.deps`, not top-level
   `agent.deps` — **validator** (unknown field).
2. `runtime.event_bus` is required when `runtime` exists — **validator**.
3. `sessions.resume: true` requires `checkpoint_store` — **validator**.
4. Graph model refs must use `model: {id: {provider, name}}`; prefer
   omitting `model` and wiring the `inference.Router` so selection runs
   per request — host build.
5. Script nodes need `runtime` and `source`; `runtime` must match the bound
   script runtime — host build.
6. Resource settings file/embed refs must be the whole settings subtree —
   host build.
7. Runtime buffer/concurrency settings have hard upper bounds —
   **validator**.
8. Dynamic catalog mappings must cover every deployed agent directly or
   with `default` — host build/runtime registration.
9. `tool.Assembly` with dynamic injection requires at least one tool
   source — host build.
10. Local sandbox is a no-isolation backend and should not be used for
    untrusted production execution.
11. `readonly_root` (or per-call `WriteReadOnly`) does not mean "no
    writes at all": explicit `writable_paths` stay writable, and on
    bwrap the private `/tmp` tmpfs is still writable (host-invisible,
    dies with the sandbox). Seatbelt has no tmpfs fallback, so it
    denies every write outside `writable_paths` and `/dev/null`. A
    `writable_paths` entry resolving to the runner root is rejected at
    build time when `readonly_root: true` — the combination is not
    silently downgraded.
12. Runtime registration cannot reuse a deployed agent name (`Conflict`);
    deployed agents can only be removed by changing the deployment
    document — runtime API.
13. With `dynamic_catalog` configured and no `default`, runtime
    registration must pass `WithToolAssembly`; otherwise registration is
    rejected up front — runtime API.
14. `UnregisterAgent` waits for active turns; a stuck engine times out
    (`WithRemoveTimeout` / ctx deadline) and the agent is left registered
    — retry, don't assume removal happened — runtime API.
15. Hardcoding `model` in inference nodes bypasses the router: no tier
    fallback, capability filtering, retry, or circuit-breaker policy
    applies. Wire `inference.Router` into the graph engine and omit
    `model` unless the deployment intentionally pins a target — host
    build/runtime.
16. `request_metadata` is only forwarded when the selected provider driver
    implements it and its spec configures `request_metadata.envelope`.
    Unsupported/unconfigured drivers do not fail the request; the metadata
    field is reported `dropped` in the compile report. Inspect
    `response.metadata.decisions` (or `Explain`) when metadata seems
    missing upstream — host build/runtime.

## Error map

| Error | Caught by | Likely cause | Fix |
| --- | --- | --- | --- |
| `config utils: decode ... unknown field "x"` | validator | extra field outside the document schema | remove it or use a documented field |
| `deployment document: version is required` | validator | missing `version: v1` | add the version field |
| `resource: kind is required` | validator | resource entry without `kind` | add `kind` |
| `runtime config: event_bus is required` | validator | missing runtime `event_bus` | add an `event.Bus` resource and reference |
| `sessions.resume requires checkpoint_store` | validator | resume enabled without store | add store or set resume false |
| graph structural errors (`entry`, duplicate node id, edge to unknown node) | validator | malformed graph definition | fix the graph JSON |
| no factory for `kind/impl` | host build | typo or missing registration | fix kind/impl and register factory |
| dep references missing resource | host build | bad resource name | use an exact resource key |
| dependency cycle | host build | circular `deps` | break the cycle |
| undeclared dep | host build | document dep not in `Spec.Deps` | fix dep name or factory spec |
| graph `unknown field "provider"` | host build | flattened model ref | use nested `id` |
| script node missing `runtime`/`source` | host build | incomplete script config | add both fields |
| dynamic catalog uncovered agent | host build/runtime | no per-agent or default mapping | add `default` or map every agent |
| `runtime: agent "x" is a deployed agent` | runtime API | register/remove collides with a document agent | register under a new name, or change the deployment |
| `dynamic catalog has no default; agent "x" needs WithToolAssembly` | runtime API | runtime registration without a tool mapping | add `WithToolAssembly` or configure `default` |
| removal `DeadlineExceeded` | runtime API | active turn did not finish in time | wait/retry; the agent is still registered |

## Debug order

1. Parse/deploy structural errors first.
2. Resolve resource factory/dependency errors.
3. Validate runtime config.
4. Validate graph node config.
