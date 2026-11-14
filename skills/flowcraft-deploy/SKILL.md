---
name: flowcraft-deploy
description: "Author, validate, and troubleshoot a FlowCraft deployment: the deployment document (agents, each with an engine plus optionally a protocol, hooks, a delegation policy and an observability level), the runtime layer one agent runs on (sandbox boundary and its journal, files, providers and models, tools and skills), and the graph document an engine of kind graph executes (state slots, nodes, the conditions on their edges). Use when writing or reviewing a FlowCraft deployment, adding a runtime layer or session settings, writing a graph document, resolving load failures (\"is not registered\", unknown field, an unenforceable cap, a missing graph path), or copying a minimal runnable FlowCraft deployment."
---

# FlowCraft Deployment Authoring

A deployment is three documents, and each one is validated without being built.

| Document | Says | Card |
| --- | --- | --- |
| `deploy.yaml` | which agents exist, each with an engine plus optionally a protocol, hooks, a delegation policy and an observability level | [references/deployment.md](references/deployment.md) |
| a runtime layer | what one agent runs on: the sandbox boundary, files, providers and models, tools, skills | [references/runtime.md](references/runtime.md) |
| a graph document | what the `graph` engine runs: state slots, nodes, the conditions on their edges | [references/graph.md](references/graph.md) |

"Not registered" nearly always means a kind, a driver, a selector or a middleware
name the host did not compile in; [references/pitfalls.md](references/pitfalls.md)
maps the messages the loader prints to their cause.

## Workflow

1. **Scope it.** Which agents, what each one is made of (a graph? a remote A2A
   peer?), and what each may touch: files, a network posture, a command boundary.
2. **Write `deploy.yaml`.** One agent is enough to start. The engine kind decides
   whether a runtime block is required: a local engine needs one, a remote engine
   must not have one.
3. **Write the runtime layer** the agent's `runtime.layers` points at. Start from
   [assets/minimal-deploy/runtime.yaml](assets/minimal-deploy/runtime.yaml):
   `files.roots`, `sandbox.backend`, one provider, one model. Every path is
   absolute, so use a placeholder (`${WORKSPACE_ROOT}`) and let the host expand it.
4. **Write the graph document** if the engine is `graph`. Start from
   [assets/minimal-deploy/graphs/assistant.yaml](assets/minimal-deploy/graphs/assistant.yaml)
   for one model call, or [assets/minimal-graph/graph.yaml](assets/minimal-graph/graph.yaml)
   for a ReAct loop.
5. **Validate.** `scripts/validate-config.sh <deployment-file>` runs the same
   loader the host does. It needs no credentials: placeholders are filled with a
   stand-in. Fix what it reports against the cards.
6. **Hand off what it could not check.** The validator builds nothing, so it does
   not prove that a driver's `settings` are meaningful, that a credential works,
   that an endpoint answers, or that a graph's node kinds exist — those are the
   host's build. Say so instead of implying the deployment is runnable.

## Validation

```bash
skills/flowcraft-deploy/scripts/validate-config.sh deploy.yaml
skills/flowcraft-deploy/scripts/validate-config.sh --type graph graphs/assistant.yaml
```

It is a small Go module pinned to a released `core` version, in
`scripts/validator/`. Run it from the directory the deployment lives in: a graph's
`settings.path` and a layer's `path` are resolved against the working directory.

What it checks, because it is the loader:

- strict decoding — an unknown field is an error, not a comment;
- `version: 1`, at least one agent, unique names;
- every `engine`/`protocol`/`hooks[].kind` is registered (the error lists what is);
- each runtime layer is merged in `priority` order and passes the full
  configuration validation: backends, net modes, cpu/memory/output budgets, the
  journal, a backend's own block, a remote transport;
- the graph document decodes, has an entry, unique node ids and edges that name
  real nodes, and its conditions compile.

What it does **not** check: provider settings semantics, credentials, endpoint
reachability, the graph's node *kinds* (a registry only exists at build time), and
anything a deployed turn does.

## The rules that bite

- **No defaults where a promise would be guessed.** `sandbox.backend` and
  `engine.kind` are required; an unknown name fails instead of being skipped.
- **Absolute paths.** `files.roots`, `sandbox.writes`, `skills.roots` and every
  other path are absolute: a deployment must not mean something different
  depending on where the process started.
- **Sequences are replaced whole when layers merge.** A profile that lists one
  writable path replaces the base list; it does not add to it.
- **A budget nothing enforces is an error.** `cpu_millicores`/`memory_mib` on a
  backend that cannot enforce them (or `cpu_millicores` without a `timeout`) fails
  the build; `writes` on `local` or `remote` only warns, because there is no local
  boundary to narrow.
- **`tools.enable` names groups** (`files`, `exec`, `skills`), not individual
  tools; the projection (`tools.dynamic`) decides what a model sees each round.
- **A graph may loop.** There is no topological order; `limits.max_steps` is what
  bounds a cycle, and an edge's condition is what leaves it.

## Compatibility

One skill version documents one `core` release: the validator's `go.mod` pins it,
and `make check-skill` fails when that pin is behind the newest `core/vX.Y.Z` tag
or when a template stops loading. When `core` releases a schema change, the pin
and the cards move in the same change.

`craft.yaml` and `plugin.json` are not part of this repository and are not covered
here.
