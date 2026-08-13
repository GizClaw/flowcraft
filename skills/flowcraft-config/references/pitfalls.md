# Pitfalls and build-failure troubleshooting

## Common pitfalls

1. Agent engine deps belong under `engine.deps`, not top-level `agent.deps`.
2. `runtime.event_bus` is required when `runtime` exists.
3. `sessions.resume: true` requires `checkpoint_store`.
4. Graph model refs must use `model: {id: {provider, name}}`.
5. Script nodes need `runtime` and `source`; `runtime` must match the bound
   script runtime.
6. Resource settings file/embed refs must be the whole settings subtree.
7. Runtime buffer/concurrency settings have hard upper bounds.
8. Dynamic catalog mappings must cover every deployed agent directly or with
   `default`.
9. `tool.Assembly` with dynamic injection requires at least one tool source.
10. Local sandbox is a no-isolation backend and should not be used for
    untrusted production execution.

## Error map

| Error | Likely cause | Fix |
| --- | --- | --- |
| no factory for `kind/impl` | typo or missing registration | fix kind/impl and register factory |
| dep references missing resource | bad resource name | use an exact resource key |
| dependency cycle | circular `deps` | break the cycle |
| undeclared dep | document dep not in `Spec.Deps` | fix dep name or factory spec |
| `runtime config: event_bus is required` | missing runtime `event_bus` | add an `event.Bus` resource and reference |
| `sessions.resume requires checkpoint_store` | resume enabled without store | add store or set resume false |
| graph `unknown field "provider"` | flattened model ref | use nested `id` |
| script node missing `runtime`/`source` | incomplete script config | add both fields |
| dynamic catalog uncovered agent | no per-agent or default mapping | add `default` or map every agent |

## Debug order

1. Parse/deploy structural errors first.
2. Resolve resource factory/dependency errors.
3. Validate runtime config.
4. Validate graph node config.
