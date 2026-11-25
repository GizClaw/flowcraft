---
name: flowcraft-deploy
description: 'Author, validate, and troubleshoot a FlowCraft deployment: the process document (the sandbox backends, model targets, tool sources, skill roots and observability level this process carries), the agent document (agents, each with a program, a grant and hooks), and the graph document a program of kind graph executes (state slots, nodes, the conditions on their edges). Use when writing or reviewing a FlowCraft deployment, adding a session-worthy authority rule, writing a graph document, resolving load failures ("is not registered", unknown field, a grant that cannot be enforced, a missing graph path), or copying a minimal runnable FlowCraft deployment.'
---

# FlowCraft Deployment Authoring

A deployment is three documents, and each one is validated without being built.
Two of them describe the deployment; the third is what one program runs.

| Document           | Says                                                                                                                                                                    | Card                                                 |
| ------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| a process document | what _this process_ carries: `sandbox[]`, `inference` (providers and route), `tools` (sources and middleware), `skills.roots[]`, and the required `observability` block | [references/runtime.md](references/runtime.md)       |
| an agent document  | what _one agent_ is: `name`, `program`, `grant`, `hooks`                                                                                                                | [references/deployment.md](references/deployment.md) |
| a graph document   | what a `graph` program runs: state slots, nodes, the conditions on their edges                                                                                          | [references/graph.md](references/graph.md)           |

`examples/forge` names the first two `runtime.yaml` and `deploy.yaml`. The names
are the application's; the loader tells them apart by decoding them, and the error
message says which one it was reading (`runtime:` or `agent:`).

The two deployment documents cannot name each other's resources. An agent selects
from the process through its `grant`; it cannot add a provider, a backend or a tool
source. Adding a resource means editing the process document and restarting, which
is a different action from deploying an agent.

"Not registered" nearly always means an `impl` name the binary did not compile in;
[references/pitfalls.md](references/pitfalls.md) maps the messages the loader
prints to their cause.

## Workflow

1. **Scope it.** Which agents, what each one is made of (a graph? something else?),
   and what each may touch: files, a network posture, a command boundary, models.
2. **Write the process document.** Start from
   [assets/minimal-deploy/runtime.yaml](assets/minimal-deploy/runtime.yaml): one
   sandbox backend, one provider, one tool source, and the two required
   `observability` fields. Every path is absolute, so use a placeholder
   (`${WORKSPACE_ROOT}`) and let the loader expand it.
3. **Write the agent document.** Start from
   [assets/minimal-deploy/deploy.yaml](assets/minimal-deploy/deploy.yaml): one
   agent, `program.settings.path` pointing at the graph, a `grant` with an explicit
   `limits.idle`.
4. **Write the graph document.** Start from
   [assets/minimal-deploy/graphs/assistant.yaml](assets/minimal-deploy/graphs/assistant.yaml)
   for one model call, or [assets/minimal-graph/graph.yaml](assets/minimal-graph/graph.yaml)
   for a ReAct loop.
5. **Check it by hand against the cards.** Every key, `impl` name and grant field
   is in the tables; the loader rejects an unknown one, so a key you cannot find is
   a key the code will not read.
6. **Hand off what a document cannot prove.** No document check builds anything, so
   nothing here proves that a settings block is meaningful, that a credential works,
   that an endpoint answers, or that a graph's node kinds exist — those are the
   host's build. Say so instead of implying the deployment is runnable.

## Validation

The shipped checker is `scripts/validate-config.sh`:

```bash
skills/flowcraft-deploy/scripts/validate-config.sh deploy.yaml
skills/flowcraft-deploy/scripts/validate-config.sh --type graph graphs/assistant.yaml
```

**Status: pinned to the previous document shape.** The validator is a small Go
module pinned to a released `core` version, and the newest released `core` predates
the process/agent split. Against the documents this card describes it therefore
fails on `program`, `grant`, `observability.bodies` and the rest — it is not a
statement about your document. It is repinned, with its checks rewritten against
the current `core` packages, once `core` is tagged again; `make check-skill` is the
lane that reports both the stale pin and the templates. Until then, treat the
tables in the references as the check.

What the loader itself guarantees, and what the validator will assert once
repinned:

- strict decoding: an unknown field is an error, not a comment;
- `version: 1` in both documents, at least one agent, unique names;
- every `impl` is registered in its group — provider, backend, tool source,
  middleware, hook, program — and the error lists what is;
- each `settings` block is decoded by its own factory, so a typo inside one fails
  here;
- the grant is normalized and enforced: `files` invariants, `exec`, `resources`,
  `limits`, `delegation`, and `escalate` against the grant it sits on;
- `grant.limits` is present and `limits.idle` is positive;
- `observability.trace` and `observability.bodies` are both present;
- every backend, model and escalation the agents name exists in this process — that
  half needs a built process, so it is `NotAvailable` rather than a document error.

What nothing checks: provider settings semantics, credentials, endpoint
reachability, the graph's node _kinds_ (a registry only exists at build time), and
anything a deployed turn does.

## The rules that bite

- **Two documents, one owner each.** A provider, backend, tool source or middleware
  is the process's; a program, grant and hooks are the agent's. A grant that names a
  backend the process did not configure is not a typo to fix in the agent document.
- **Required where a default would be a guess.** `observability.trace` and
  `observability.bodies` must both be written; so must `grant.limits.idle`. There
  is no inherited record level and no inherited idle timeout, because "we keep
  nothing" and "nobody said" are different facts.
- **`grant` is a ceiling, not a request.** `Grant.Narrow` produces a session's
  authority from it; a caller sends models, tools and a budget, never a path or a
  network posture. A request outside the ceiling is refused, never truncated.
- **`escalate` is a declared widening, and it needs a boundary.** Only
  `files.write`, `net` and `exec.env` can widen, `ttl` and `max_per_session` are
  required, and the selected backend must confine writes — `local` does not, so
  `escalate` on it is `NotAvailable`, not a warning.
- **`files.readonly` is a tool-level permission.** It does not stop
  `exec("echo x > /tmp/f")`; that needs a backend that mounts read-only or confines
  writes.
- **Absolute paths, no globs.** File roots are absolute, whitespace-free and
  literal; a root must already exist. `read` narrowed while `write` stays open is
  refused rather than repaired.
- **Sequences are replaced whole when layers merge.** An overlay that lists one
  agent replaces the `agents` list; there is no per-agent patch.
- **A name means something only inside its group.** `local` as a backend and
  `local` as a provider are two registrations.
- **A graph may loop.** There is no topological order; `limits.max_steps` is what
  bounds a cycle, and an edge's condition is what leaves it. The reserved edge
  target is lowercase `end`.

## Compatibility

One skill version documents one `core` release: the validator's `go.mod` pins it,
and `make check-skill` fails when that pin is behind the newest `core/vX.Y.Z` tag
or when a template stops loading. When `core` releases a schema change, the pin
and the cards move in the same change — which is the state described under
[Validation](#validation) today.

`craft.yaml` and `plugin.json` are not part of this repository and are not covered
here.
