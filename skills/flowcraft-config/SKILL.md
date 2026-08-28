---
name: flowcraft-config
description: "Author, validate, and troubleshoot complete FlowCraft deployment configuration (deploy.yaml with the runtime section, inference/workspace/sandbox/tool sub-documents, core/memory contracts, and graph JSON node wiring). Use when writing or reviewing FlowCraft configs, assembling an agent deployment, adding runtime/session settings, building graph definitions, resolving config build failures (\"not registered\", dead configuration, route policy missing, graph node errors), or copying a minimal runnable FlowCraft deployment template."
---

# FlowCraft Config Authoring

Write complete FlowCraft deployment configuration: `deploy.yaml`, the
`runtime` section, resource sub-documents, and graph JSON. Validate with
the L2 structural validator and fix errors against the reference cards.

## Workflow

1. **Scope the deployment.** Collect the agents, capabilities (chat,
   tools, memory, scripts, sandboxing), and runtime needs (sessions,
   resume, event bus, checkpoint stores).
2. **Draft the deployment document.** `deploy.yaml` is the convention
   but any filename works; pass whatever path you choose to the
   validator. Read
   [references/deploy.md](references/deploy.md) first. Order the areas:
   resources → agents → runtime. Decide whole-resource vs item dep refs
   (`infer` vs `ws/project`).
3. **Write sub-documents.** Read
   [references/resources.md](references/resources.md) for the owning
   module's schema. Workspace before sandbox (custom sandbox runners may
   depend on workspace resources). Memory implementation modules are
   app-registered — omit implementation examples; `core/memory` contracts
   and hooks are fine to use.
4. **Write graph JSON.** Read [references/graph.md](references/graph.md).
   Prefer routing: wire the `inference.Router` into the graph engine and
   omit `model` in inference nodes; pin `model` only when no router is
   wired. Model refs must use the nested `id` form; script nodes need
   `runtime` and `source`; wire edges back to the inference node after
   tool nodes.
5. **Validate structurally with L2.** Run
   `skills/flowcraft-config/scripts/validate-config.sh <deployment-file>`
   (or the installed copy's script). The validator pins the FlowCraft
   core module in its `go.mod` and works standalone from any directory.
   It is structural only: it strictly decodes the document through
   `core/deploy.Parse` (unknown fields are rejected, resource/agent
   entries are shape-checked), strictly decodes and validates the
   `runtime` subtree through `core/runtime.DecodeConfig` when present,
   and structurally validates graph definitions through
   `core/graph.GraphDefinition.Validate`. It does not build resources:
   no factory registry, no settings `file`/`embed` resolution, no node
   config decoding, no credentials, and no provider calls. Custom and
   app-registered kinds pass as long as they fit the resource envelope;
   their settings semantics are the host build's job.

   Sub-documents can also be validated on their own:
   `validate-config.sh --type inference inference.yaml` (types:
   `deploy`, `inference`, `workspace`, `sandbox`, `tool`, `graph`,
   `agent`). Standalone types are parsed as JSON/YAML with strict YAML
   conversion (duplicate keys rejected, single document enforced);
   `graph` additionally runs `GraphDefinition.Validate` (unique node
   ids, entry presence, edge endpoints). No sub-document schema is
   decoded in standalone mode — `inference`, `workspace`, `sandbox`,
   `tool`, and `agent` are syntax checks only.
6. **Fix errors.** Each failure is prefixed `[parse]`, `[graph]`,
   or `[build]` (`[parse]` input/syntax errors, `[graph]` graph
   definition errors, `[build]` deploy document or runtime subtree
   errors); validation stops at the first error.
   Consult [references/pitfalls.md](references/pitfalls.md) for the
   error → cause map. The validator prints no warnings: custom and
   app-registered kinds pass structurally, and settings semantics it
   cannot check are the host build's responsibility.
7. **Hand off.** Report the structural validation result and what L2
   could not verify: settings schemas, factory resolution, `file`/`embed`
   references, and graph node configs are validated only when the host
   builds the deployment with its own registry. Note the registration
   code the host still needs. If the deployment relies on runtime agent
   registration, call out that it needs the live registry API
   (`Runtime.RegisterAgent` / `UnregisterAgent`; see
   [references/runtime.md](references/runtime.md)) and any
   `dynamic_catalog` `default`/`WithToolAssembly` requirement.

## Cross-file invariants

These invariants describe core semantics. The validator enforces the
structural subset (see step 5); the rest are enforced when the host
builds the deployment with its own factory registry.

- Memory hooks bind a whole `memory.Assembly` resource; the settings
  schema is impl-owned. Concrete implementations are app-registered.
- Sandbox runners are `sandbox.Runner` resources; first-party impls
  (`local`, `bwrap`, `seatbelt`) take no deps, custom impls declare
  their own.
- `runtime.event_bus` is required when a `runtime` section exists, and
  `event_bus`/`checkpoint_store`/`dynamic_catalog.tools` values must name
  resources in the document (the host build resolves them by contract).
- `sessions.resume: true` requires `checkpoint_store`.
- Agent `tools` is an allow-list, not a catalog declaration; `policy` is
  harness state, not engine settings.
- Graph `model` refs: `model: {id: {provider, name}}`.
- Runtime-registered agents reuse the deployment assembly path; their
  names must not collide with deployed agents, and with a `dynamic_catalog`
  they need a tool mapping or a `default`.

## Templates

Copy [assets/minimal-deploy](assets/minimal-deploy/) as a starting point:
one agent, graph inference node, and a validated runtime section. Replace
the model/provider references and workspace root, then extend per the
workflow.

## Compatibility and versioning

One skill version pins one FlowCraft version. The validator's `go.mod`
requires exactly `github.com/GizClaw/flowcraft/core v0.1.28`; the schema
cards in this skill document that release (no `driver/*` or `backends/*`
modules are required, since the validator never constructs factories).
When FlowCraft releases a new version, bump the pin and reconcile the
cards in the same change. Custom kinds are structurally valid by design;
their factories and settings schemas live in the host application.

## Reference index

- [deploy.md](references/deploy.md) — deploy document schema, dep refs,
  first-party kinds, registration.
- [runtime.md](references/runtime.md) — runtime section, sessions,
  dynamic registration, reload.
- [resources.md](references/resources.md) — inference/memory/workspace/
  sandbox/tool/event/checkpoint/delegation/script-runtime sub-documents.
- [graph.md](references/graph.md) — graph JSON, node configs, engine
  build settings.
- [pitfalls.md](references/pitfalls.md) — known drift points and the
  error → cause map.
