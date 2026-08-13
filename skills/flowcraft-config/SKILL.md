---
name: flowcraft-config
description: Author, validate, and troubleshoot complete FlowCraft deployment configuration (deploy.yaml with the runtime section, inference/workspace/sandbox/tool sub-documents, core/memory contracts, and graph JSON node wiring). Use when writing or reviewing FlowCraft configs, assembling an agent deployment, adding runtime/session settings, building graph definitions, resolving config build failures ("not registered", dead configuration, route policy missing, graph node errors), or copying a minimal runnable FlowCraft deployment template.
---

# FlowCraft Config Authoring

Write complete FlowCraft deployment configuration: `deploy.yaml`, the
`runtime` section, resource sub-documents, and graph JSON. Validate with
the L2 dry-run harness and fix build failures against the reference cards.

## Workflow

1. **Scope the deployment.** Collect the agents, capabilities (chat,
   tools, memory, scripts, sandboxing), and runtime needs (sessions,
   resume, event bus, scheduler, integrations).
2. **Draft the deployment document.** `deploy.yaml` is the convention
   but any filename works; pass whatever path you choose to the
   validator. Read
   [references/deploy.md](references/deploy.md) first. Order the areas:
   resources → agents → runtime. Decide whole-resource vs item dep refs
   (`infer` vs `ws/project`).
3. **Write sub-documents.** Read
   [references/resources.md](references/resources.md) for the owning
   module's schema. Workspace before sandbox (sandbox deps a workspace
   registry). Memory implementation modules are app-registered and the
   concrete memory implementations are app-registered — omit
   implementation examples; `core/memory` contracts and hooks are fine to use.
4. **Write graph JSON.** Read [references/graph.md](references/graph.md).
   Model refs must use the nested `id` form; script nodes need `runtime`
   and `source`; wire edges back to the inference node after tool nodes.
5. **Validate with L2.** Run
   `skills/flowcraft-config/scripts/validate-config.sh <deployment-file>`
   (or the installed copy's script). The validator pins the FlowCraft
   module versions in its `go.mod` and works standalone from any
   directory. It parses every sub-document, dry-builds the deployment
   through the real `core/deploy` assembly with first-party factories
   and stub secrets, decodes graph node configs statically, and
   cross-checks the runtime section. It never reads real credentials and
   never calls providers.

   Sub-documents can also be validated on their own:
   `validate-config.sh --type inference inference.yaml` (types:
   `deploy`, `inference`, `workspace`, `sandbox`, `tool`, `graph`,
   `agent`). Standalone checks use each module's real parser; graph and
   inference additionally run their semantic checks (node config decode,
   route targets against the provider catalog).
6. **Fix errors.** Each failure is prefixed `[parse]`, `[graph]`,
   `[build]`, or `[runtime]`; the build short-circuits on the first error.
   Consult [references/pitfalls.md](references/pitfalls.md) for the
   error → cause map. Treat `warnings:` as review items: scope notes name
   app-registered kinds the harness cannot construct, and tool allow-list
   warnings flag names missing from the built catalog.
7. **Hand off.** Report what was assembled, what L2 could not validate
   (app-registered kinds/hooks/sources), and the registration code the
   host still needs.

## Cross-file invariants

- Memory hooks bind a whole `memory.Assembly` resource; the settings
  schema is impl-owned. Concrete implementations are app-registered.
- Sandbox resources need `deps: {workspaces: <workspace resource>}`.
- `runtime.event_bus` is required when a `runtime` section exists; every
  runtime resource/integration dep name must be an exact resource key.
- `sessions.resume: true` requires `checkpoint_store`.
- Agent `tools` is an allow-list, not a catalog declaration; `policy` is
  harness state, not engine settings.
- Graph `model` refs: `model: {id: {provider, name}}`.

## Templates

Copy [assets/minimal-deploy](assets/minimal-deploy/) as a starting point:
one agent, graph inference node, and a validated runtime section. Replace
the model/provider references and workspace root, then extend per the
workflow.

## Compatibility and versioning

One skill version pins one FlowCraft version. The validator's `go.mod`
requires `github.com/GizClaw/flowcraft/core v0.1.0` plus the relevant
`driver/*` and `integrations/*` modules; the schema cards in this skill
document exactly that release. When FlowCraft releases a new version,
bump the pins and reconcile the cards in the same change. The L2 harness
registers the first-party surface only; document anything app-registered
in the handoff.

## Reference index

- [deploy.md](references/deploy.md) — deploy document schema, dep refs,
  first-party kinds, registration.
- [runtime.md](references/runtime.md) — runtime section, sessions,
  integrations.
- [resources.md](references/resources.md) — inference/memory/workspace/
  sandbox/tool/event/scheduler/checkpoint sub-documents.
- [graph.md](references/graph.md) — graph JSON, node configs, engine
  build settings.
- [pitfalls.md](references/pitfalls.md) — known drift points and the
  error → cause map.
