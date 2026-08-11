# Pitfalls and build-failure troubleshooting

These are the drift points found while aligning the guides with the
modules. When a config fails, check this list before inventing new schema.

## Known pitfalls

1. Memory hooks (`memory.context` / `memory.turn`) bind a whole
   `memory.Assembly` resource as their `memory` dep; the settings schema
   is owned by the implementation module. The flowcraft `memory/` module
   is not released yet — do not write memory implementation examples
   against it. `sdk/memory` contracts and `sdkx/memory/hook` are fine.
2. The default credential profile has an empty id (`- secrets: ...`),
   not `id: default`.
3. bytedance transcription is session-only (no unary transcribe); azure
   supports unary transcription and image/audio intents; openai realtime
   is absent.
4. The transcription span name is `inference.transcription`, not
   `inference.transcribe`.
5. Agent `tools` is a runtime allow-list promoted to `Run.ToolAllowList`;
   the deploy document does not validate names against the catalog.
6. Agent `policy` is read by the harness, not the engine factory: it maps
   to `WithMaxRevise` / `WithArtifactChannels`.
7. `workspace` scope semantics: reads default to allowed (deny-only
   `deny_read`), writes default to denied (allow-only `allow_write`).
8. The event `Observer` has only `OnPublish` / `OnDeliver` / `OnDrop`;
   there is no subscriber-count or cache hit-miss callback.
9. `sdkx/tool/exec` also ships `exec_session` alongside `exec`.
10. The event `BackpressurePolicy` includes `Sample`.
11. Runtime config supports `checkpoint_store` and `sessions.resume`
    (resume requires the store).
12. `EventBus()` is an optional `agent.EventBusProvider`, not a member of
    the `Host` interface.
13. Workspace/sandbox deploy factories are the builders themselves:
    register `workspaceconfig.NewBuilder(...)` and
    `sandboxconfig.NewBuilder(...)` directly; there is no
    `workspaceconfig.NewDeployFactory`.
14. Graph `model` refs need the nested `id`:
    `model: {id: {provider: ..., name: ...}}`; the flattened
    `{provider: ..., name: ...}` form is invalid.
15. `LocalWorkspace.Capabilities` reports `DurableOnWrite: false`.

## Error → cause map

| Error you see | Likely cause | Fix |
| --- | --- | --- |
| memory implementation build error | impl-owned settings mismatch or missing deps | check the implementation module's schema; bind the whole assembly the hooks require |
| `no constructor registered for kind "X" impl "Y"` | typo, or app-registered kind | fix kind/impl; verify host registration (L2 reports this as a scope limit) |
| `dead configuration` | resource built but consumed by nothing | bind it, set `export: true`, or mark it as an external consumer |
| `runtime config: sessions.resume requires checkpoint_store` | resume enabled without store | add `checkpoint_store` resource + runtime field, or set `resume: false` |
| `runtime references resources that are not in the deployment` | runtime name mismatch | resource keys must match exactly |
| `unknown field "..."` | typo at any level | check the owning section's schema |
| `invalid node config: json: unknown field "provider"` | flattened model ref | use `model: {id: {provider, name}}` |
| `script node: runtime is required` / `source is required` | script node missing fields | set `runtime` and `source` |
| `graph entry node "X" not found` | `entry` mismatch | fix `entry` or node ids |
| `edge to unknown node "X"` | edge target typo | targets must be node ids or `__end__` |
| agent tool allow-list warning | tool not in built catalog | app-registered tool (expected) or typo in the name |
| `provider driver "X" is not registered` | driver typo or custom driver | use a first-party driver name or register the custom factory |
| `secret resolver "X" is not registered` | resolver typo or app-owned resolver | use `env` or register the resolver in the host |

## Debug order

1. `deploy.Parse` errors are structural — fix top-level typos first.
2. `Builder.Build` errors short-circuit on the first failure — fix the
   reported resource/agent, then re-run.
3. Runtime errors come after a successful build — check name wiring and
   the resume/checkpoint invariant.
4. Graph node config errors surface at invocation, not at build. The L2
   validator decodes node configs statically to catch them early; keep
   model refs nested and script nodes complete.
