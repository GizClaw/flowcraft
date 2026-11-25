# Pitfalls

Every message below is one the loader prints, and its cause. The loader is the
authority: an entry in a document is either read by the code or refused, never
silently ignored.

## Two classes of failure

| Class          | Means                                                                                                                                                                          | Fixed by                          |
| -------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | --------------------------------- |
| `Validation`   | The document does not hold together: unknown field, wrong type, missing required value, duplicate name, an `impl` nobody registered, a grant that cannot be enforced.          | Editing the document.             |
| `NotAvailable` | The document holds together but asks for something this process lacks: a backend it did not configure, a model it does not serve, an escalation on a backend with no boundary. | Changing how the process started. |

The prefix says which document was being read: `runtime:` for the process
document, `agent:` for the agent document.

## Decoding

| Message                                            | Cause                                                                                                                                                  |
| -------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `agent: json: unknown field "engine"`              | An agent is `name`, `program`, `grant`, `hooks`. `engine`, `runtime`, `protocol`, `catalog` and an agent-level `observability` are the previous shape. |
| `runtime: json: unknown field "models"`            | Models live under `inference.providers[].settings`, or in a grant's `models`; the process document has no top-level `models`.                          |
| `runtime: document version 2, want 1`              | `version` is the integer `1` — not `"v1"`, not `2`.                                                                                                    |
| `agent: document version 0, want 1`                | The field is missing entirely.                                                                                                                         |
| `deploy: settings: json: unknown field "thinking"` | A `settings` block is decoded strictly by its own factory; the key is not one that implementation reads.                                               |

## Process document

| Message                                                                                                           | Cause                                                                      |
| ----------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------- |
| `deploy: observability.trace is required (none, run, call or node)`                                               | The process must say how much it records. `off` is not a level; `none` is. |
| `deploy: observability.bodies is required (true or false)`                                                        | Absent is not "false": retention of model answers is written down.         |
| `deploy: provider "luna" selects impl "opnai", which is not registered (anthropic, openai)`                       | The binary did not compile that provider in; the list is what it did.      |
| `deploy: provider "luna" is declared twice`                                                                       | Two entries share a name, and a name is how a grant selects the resource.  |
| `deploy: a provider entry needs a name`                                                                           | An entry with `impl` but no `name`.                                        |
| `deploy: middleware "telemetry" selects impl "telemetri", which is not registered (recover, telemetry, truncate)` | Same rule for a chain link.                                                |
| `deploy: route.attempts_per_target must not be negative`                                                          | A retry count below zero.                                                  |

## Agent document

| Message                                                                                                                          | Cause                                                                                      |
| -------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------ |
| `deploy: an agent document needs at least one agent`                                                                             | `agents` is empty.                                                                         |
| `deploy: every agent needs a name`                                                                                               | An agent without `name`; the address is what a host and an overlay both use.               |
| `deploy: agent "x" is declared twice; a document names each agent once`                                                          | Two entries share a name.                                                                  |
| `deploy: agent "x" must state grant.limits (idle, and optionally max_sessions)`                                                  | No `limits` block. There is no inherited idle timeout.                                     |
| `deploy: agent "x": grant.limits.idle must be a positive duration`                                                               | `limits.idle: 0` or a negative duration.                                                   |
| `deploy: agent "x": program selects impl "grpah", which is not registered (graph)`                                               | The process did not register that program.                                                 |
| `deploy: agent "x": policy.files.read: path "out" must be absolute`                                                              | A file root, or an `escalate.files.write` root, that is relative or a glob.                |
| `deploy: agent "x": policy.files: readonly and write are mutually exclusive (write: [out])`                                      | `files.readonly: true` beside a non-empty `write`.                                         |
| `deploy: agent "x": policy.files: read is narrowed to [work] but write is unrestricted; declare write or readonly`               | A read root with no write root and no `readonly`.                                          |
| `deploy: agent "x": policy.files: "/srv/out" is writable but not readable; write must be a subset of read`                       | A write root that no read root covers.                                                     |
| `deploy: agent "x": policy.escalate: max_per_session must be positive`                                                           | An escalation nobody may use, or a missing counter.                                        |
| `deploy: agent "x": policy.escalate: files.read is not escalatable; an escalation widens writes only`                            | Only `files.write`, `net` and `exec.env` widen.                                            |
| `deploy: agent "x": policy.escalate: files.write is declared but the grant's write set is already unrestricted`                  | There is nothing to widen, and a root list would shrink it.                                |
| `deploy: agent "x": policy.escalate: files.write [] does not cover the grant's own write set [/srv/agent/out]`                   | An escalation only widens: on a grant with a restricted write set it restates those roots. |
| `deploy: agent "x": policy.escalate: net does not cover the grant's own posture; an escalation only widens`                      | The declared posture is narrower than the grant's, so it would shrink it.                  |
| `deploy: agent "x": policy.escalate: exec.env.allow must name the variables to widen to (null means the whole host environment)` | An `exec` block with no allow-list.                                                        |
| `deploy: agent "x": policy.delegation: at least one mode is required`                                                            | `delegation` present with `targets` but no `modes`.                                        |
| `deploy: agent "x" selects sandbox "bwrap", which this process does not configure (local)`                                       | `NotAvailable`: the process has no such backend.                                           |
| `deploy: agent "x" names model "openai/gpt-5", which this process does not serve (openai/gpt-4o-mini)`                           | `NotAvailable`: the grant names a model no provider catalog serves.                        |
| `deploy: agent "x" declares grant.escalate on sandbox "local", which does not confine writes; there is no boundary to widen`     | `NotAvailable`: `local` confines nothing, so nothing can be exceeded.                      |
| `deploy: agent "x" declares grant.escalate but selects no sandbox; there is no boundary to widen`                                | An escalation with no backend named.                                                       |

## Graph document

| Message                                                   | Cause                                                                                                                                           |
| --------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| `graph: entry is required`                                | Nothing to start from.                                                                                                                          |
| `graph: node "model" is declared twice`                   | Node ids are the edge endpoints and must be unique.                                                                                             |
| `graph: node "end" is reserved`                           | `end` is the reserved edge target, not a node id.                                                                                               |
| `graph: edges[0]: unknown target "END"`                   | The reserved target is lowercase `end`.                                                                                                         |
| `graph: node "model": kind "modle" is not registered`     | An unknown node kind; the registry is built by the host, so a standalone check cannot know it.                                                  |
| `graph: edges[0]: <compile error>`                        | A condition is compiled when the graph is, so a typo is a load error.                                                                           |
| `read ./graphs/assistant.yaml: no such file or directory` | `program.settings.path` resolves against the process working directory, not against the agent document. Start the host from the workspace root. |

## Shapes that load but do not work

| Symptom                                       | Cause                                                                                                                      |
| --------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| A turn runs but the model has no tools        | The grant's `tools.allow` is set and does not list them, or their source is not in `tools.sources`.                        |
| `writes` has no effect                        | `sandbox: local` has no boundary. It refuses a write-restricting confine rather than accepting one it cannot enforce.      |
| Every command is refused                      | An `Approval` policy with neither a whitelist entry nor an approver refuses everything: "I have not decided" means no.     |
| A refused command is never offered a widening | `grant.escalate` is absent, or no `Escalator` is installed — the latter fails with `NotAvailable`.                         |
| A graph loops forever                         | `limits.max_steps` is unset, so only the caller's context bounds it.                                                       |
| A carried state key grows every turn          | `carry: true` re-reads the key from the store each turn; a long conversation belongs in one task, not in a carried key.    |
| Two agents disagree about a record level      | They cannot: `observability` is the process's, and a level two agents could disagree about would be a knob with no effect. |
