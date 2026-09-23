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
17. No driver ships a built-in line-up, so a model declaration is the whole
    fact: an omitted leaf is undeclared, never inherited from a same-named
    model. A generate model must state `outputs: [text]`, and the `catalog`
    key that used to select a namespace is rejected with the migration path.
    Driver control facts without a capability kind of their own are
    declaration leaves too (Bytedance `max_resolution`/`video`, MiniMax
    `wire_model`/`video`); leave one out and the endpoint's own validation
    decides instead of the driver rejecting locally — host build.
18. `effort_none` (OpenAI/Azure) and the top-level `dimensions:` key
    (OpenAI/Azure/Bytedance) were removed: strict decoding reports them as
    unknown fields. OpenAI-family reasoning off is implied by
    `reasoning: toggle`; embed custom dimensions now live under
    `capabilities.custom_embed_dimensions` — host build.
19. With dynamic injection, `tool_search` must be exposed as `always`.
    Configuring it as `direct`/`deferred`/`hidden` disables discovery and
    is rejected at host build; omit it from `exposures` (it is forced to
    `always`) or set it explicitly. `tool_search` no longer accepts a
    `select` argument — matching tools auto-expose from the query hits.
20. `build.max_iterations: 0` means *unlimited*, not "use the default": omit
    the key for the default `100`. The cap counts nodes *routed*, so a skipped
    node still consumes budget, and a wave that no longer fits fails as a
    whole (HTTP 429) rather than running partially — raise the cap or exit the
    loop with a condition on `__iterations` — host build/runtime.
21. `build.timeout` and `build.max_iterations` are per execute call: with
    `policy.max_revise: N` the worst case is N × that budget. Set
    `policy.run_timeout` (e.g. `10m`) to bound the whole run, attempts
    included — host build/runtime.
22. `max_iterations` is a loop guard, not a cost guard: `max_node_retries`
    attempts and provider calls made inside one node (a script looping over
    `inference.generate`) do not advance it. Cross-attempt token/cost limits
    belong in the host usage budget (`agent.Host.ReportUsage`) — runtime.
23. A sandbox `journal:` is observation, not enforcement: it reports the
    writes that happened (`create` / `write` / `rename` / `remove`), it
    never blocks one, and it is bounded — read with a cursor and treat a
    non-nil gap as "this may be incomplete". Reads and `chmod`s are never
    events, and writer attribution (`WriteEvent.Session`) is empty on
    every platform, so a turn's writes cannot be attributed by the
    journal alone. The sources are inotify (Linux, one watch per
    directory), kqueue (macOS, one descriptor per watched entry, and the
    soft `RLIMIT_NOFILE` is raised toward the watch budget when the
    journal is attached) and ReadDirectoryChangesW (Windows, one watch
    per directory plus the 16 KiB change buffer the kernel pins for its
    pending read — the budget there buys memory, so `max_watch_set` is
    worth setting; it is validated against a 1048576-watch cap, as
    `retention` is against 1048576 events, so a typo fails the host
    build rather than the host); on macOS and Windows there is no close
    edge, so an appearance or a content change is reported once it
    settles rather than at the writer's close. Configuring `journal:`
    on a platform without a source (the BSDs today) fails the host
    build with NotAvailable instead of watching nothing — host
    build/runtime.

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
| `tool: tool_search must be registered with exposure "always"` | host build | `tool_search` explicitly set to a non-always exposure | remove it from `exposures` or set `tool_search: always` |

## Debug order

1. Parse/deploy structural errors first.
2. Resolve resource factory/dependency errors.
3. Validate runtime config.
4. Validate graph node config.
