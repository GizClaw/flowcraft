---
layout: default
title: Process and Turn Runtime
---

# Process and Turn Runtime Guide

A FlowCraft process is built once and then serves conversations from it. Two
values own that split, and both are `core/host`:

| Value           | Lives as long as   | Holds                                                                                                                                                       |
| --------------- | ------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `host.Resource` | the process        | the providers and the routed provider, the backend factories, the skill index, the tool registry, the dispatcher, the observability level, the view builder |
| `host.Agents`   | the deployment set | the store, the fingerprint, approval and the escalator, the deployment table, the session table                                                             |

Neither is a `runtime` package and neither is a `Runtime` type: the process is
`host.Resource`, and a conversation is `host.Session`. There is no per-agent
runtime — the record level, the providers, the backends and the tool table are
built once for the process, because a level two agents could disagree about would
be a knob with no effect.

## Building the process

```go
plan, err := loader.Process(ctx, sources.File("runtime.yaml"))

res, err := host.Open(ctx, plan, host.Options{
    // The capability slots this process declares; the host fills them per session.
    Capabilities: capSet,
    // The tool executor this binary compiles in, over the process tool table.
    Dispatcher: dispatcher,
    // The projection: what a deployment's grant allows, as a factory per session.
    View: view,
})
defer res.Close()
```

`host.Open` builds in dependency order — the skill index first, then the
providers, then the routed provider over them, then the backends, then the tool
sources, then the middleware chain — and records a closer per resource. A failure
runs the closers in reverse and joins their errors rather than replacing the
cause. The routed provider is built even for a single target: one seam that always
routes keeps callers from branching on "is there more than one".

Two options are the application's decision rather than the document's, which is why
they are Go values:

- `Dispatcher` — which executor runs a tool call. `tool/dispatch` is the shipped
  one; the chain it wraps is the document's `tools.middleware`.
- `View` — the projection. It takes the catalog and the tool set a grant allows, and
  returns a `tool.ViewFactory`. One view per session is not a detail: a shared view
  would let two conversations evict each other's tools and make one session's
  prompts depend on what another session did.

`res.Capabilities()` is what the built process reports — backend names, their
enforcement, and the models its provider catalogs serve as `"provider/name"` —
and it is the value an agent document is resolved against.

## The session is where the four nouns meet

```go
session, err := agents.Open(ctx, sessionID, "assistant",
    authority, // a policy.Grant: what this session may do
)
for event, err := range agents.Run(ctx, sessionID, msg) { ... }
```

| Noun        | Owns                                                                                           |
| ----------- | ---------------------------------------------------------------------------------------------- |
| the agent   | the deployment: its program, its hooks, its session table.                                     |
| the cell    | the boundary: one backend instance built from _this_ session's confine, plus its `fsx.Tree`.   |
| the binding | one `tool.Binding`: the session's grant, its identity, the view, the capability pocket.       |
| the driver  | one `agent.Driver` over the deployment's program: the in-flight turn, its queue, its cancel.   |

`Open` is admit → build outside the table's lock → re-validate → insert. Only the
cheap questions are asked under the lock (closed, unknown agent, session limit,
duplicate id); the expensive work — normalizing the grant, checking it against what
the process carries, `openCell`, capability slots, the view, the manifest,
`program.Open`, the `OnSession` hook — happens without it, and the same questions
are asked again before the insert. The session limit must be checked under the lock
that inserts: two opens can both pass the first check.

The grant is a required argument, so there is no default to inherit: the deployment
document says what runs, and the caller says what this conversation may do. A grant
naming a backend or a model the process does not carry is `NotAvailable`; one that
does not validate is a `Validation` error. Neither is truncated into something that
looks narrower but is not.

## The boundary comes from the grant

```text
bind (a policy.Grant)      → confineOf      → sandbox.Confine
                           → factory(confine) → sandbox.Runner   (one per session)
                           → ValidateConfine  → refused if unenforceable
bind.Files                 → fsx.Open         → the session's tree
runner + approval          → platform.ProcessSlot
```

- **The confine is a projection, not a second spelling.** Every field comes from
  the grant unchanged, and the backend re-validates it when the instance is built,
  so a backend that skipped its own check cannot hand out a boundary it does not
  enforce.
- **`sandbox.ValidateConfine` is the construction-time gate.** A network posture
  the backend does not list, an environment allow-list it cannot apply, a
  write-restricting confine on a backend with no filesystem bounds, or a resource
  budget it cannot cap is refused here — at open, where an operator sees it.
- **`tool.Binding` is assembled by the host and never read from a context.** A tool
  cannot be handed a grant its caller never granted; `tool.Bind(session, grant)` is
  the one way to a valid binding.
- **`local` confines nothing.** It runs through `os/exec` with no filesystem
  boundary and refuses a write-restricting confine instead of ignoring one; a
  stronger promise needs a backend that mounts or sandboxes.

## One turn, end to end

```text
host.Run → Agents.request → agent.RunTurn → Driver (graph, a2a)
    → inference.Provider
    → tool.Dispatcher → sandbox.Exec → the driver
    → finish: metrics, checkpoint, run record, hooks
```

| Step             | What it does                                                                                                                                  |
| ---------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| `Agents.Run`     | Claims the turn slot — a second run is `Conflict`, a closing session `NotAvailable` — then derives the run context from the session identity. |
| `Agents.request` | Counts the ordinal, loads the session's latest task, mints a task id when the last one is terminal, and assembles the `agent.Request`.        |
| `agent.RunTurn`  | Installs the failure box, puts the trace level in the context, opens `agent.turn`, runs `BeforeTurn`, then folds the driver's stream.         |
| the driver       | Calls the routed provider and the dispatcher; every tool call reaches `sandbox.Exec` through the session's runner.                            |
| `agent.finish`   | Classifies the outcome, records metrics, writes the terminal checkpoint, reports the run record, then runs `AfterTurn`.                       |

Three properties are load-bearing:

- **One turn per session.** `beginTurn` refuses a second claim, which is what lets
  `Steer` and `Cancel` address the turn in flight without an id.
- **The terminal save happens on every path** — a normal finish, a panic, a
  consumer that stopped, and a `BeforeTurn` that refused. A turn that never started
  still leaves a terminal checkpoint.
- **A session pins its deployment.** `Reload` swaps the table entry and marks the
  previous program retired; it is closed when its last session is gone.

## Four channels, four jobs

| Channel         | Written by                                                | Read by                                        | Never carries                                                       |
| --------------- | --------------------------------------------------------- | ---------------------------------------------- | ------------------------------------------------------------------- |
| stream events   | the driver, forwarded unchanged by `RunTurn`              | the caller of `Run` — a UI, a protocol adapter | persistence: the stream is the product surface                      |
| spans           | `telemetry.Start` at each boundary                        | a trace backend, someone debugging a run       | streamed text or reasoning; an error's _message_                    |
| metrics         | host, agent, tool chain, inference pipeline               | dashboards, alerts                             | per-record identifiers (`session.id`, `task.id`, `tool.call_id`, …) |
| durable records | the turn decorator and the drivers, through `DriverStore` | resume, audit, tools                           | handles, connections, command text                                  |

The order inside `finish` is deliberate: metrics first, then the terminal
checkpoint, then the run record through `OnRun`, then `AfterTurn` — so a sink and a
hook see the same durable answer, and a hook failure is logged without breaking the
stream.

## Observability is a process fact

```yaml
observability: { trace: call, bodies: false }
```

Both fields are required. A process that left one out would inherit something, and
there is nothing to inherit: "we record nothing" and "nobody said" are different
facts.

| Level  | Adds                                                                          |
| ------ | ----------------------------------------------------------------------------- |
| `none` | the run and its turns                                                         |
| `run`  | one span per driver step, plus the turn's `checkpoint.save`                   |
| `call` | the per-call spans: a model call, a tool call, one command                    |
| `node` | whatever the driver considers a node of its own — the graph adds one per node |

`bodies` is retention, not a level: it decides whether a model response's bytes are
kept for replay. The turn writes the answer to the ledger only when it is set, so
the same retention decision is not made twice.

A middleware factory declares `AtTrace` — the least level it is worth installing at
— and the loader leaves a link out of the plan when the process records less than
that, instead of building one that turns itself off per call.

## Two command rules

A command reaches the backend through `platform.ProcessSlot`, which the host fills
with the session's runner decorated by the process's command rules.

**Approval** answers "may this run at all". A `host.Approval` has two rules, in
this order: a command the `Whitelist` covers runs without asking (argv prefix, word
by word; an empty entry matches nothing), and everything else goes to the
`Approver`. It is fail-closed — a policy with neither rule refuses every command,
and an approver error is not a denial and also refuses. A process with no policy
installs the starter undecorated, so approval stays opt-in.

**Escalation** answers "may this one command exceed the boundary". Only a command
the backend already refused with `sandbox.ErrConfineDenied` is offered, and there is
exactly one retry:

```go
agents := host.NewAgents(res,
    host.WithApproval(host.Approval{Whitelist: host.Whitelist{{"git", "status"}}, Approver: ui}),
    host.WithEscalation(ui),              // implements host.Escalator
    host.WithEscalationTTL(5*time.Minute), // how long the widened cell may live
)
```

The escalator is told the command, its working directory and the authority in force
(`From`). Its answer goes through `Grant.Widen`, so an answer that drops a permit the
session had, changes anything but `files.write`, `net` and `exec.env`, or does not
validate is refused rather than trusted. The widened command runs in a temporary
cell built from the wider grant — the session's own cell is never widened in place,
because a boundary is what a session _is_ — and `ValidateConfine` is what says
whether the backend can honour it. The audit record carries hashes and digests
(command hash, both bind digests, the escalator's type, the decision, the duration)
under event kind `escalation`, on `context.WithoutCancel`, so a cancelled turn still
leaves the record.

With no escalator installed the refusal stands: the feature is opt-in, like approval.

## The session table

| Call                                     | Use                                                                                                                                                         |
| ---------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Agents.Open`                            | Admit a session under the grant the caller states; `WithChain` attaches a delegation lineage.                                                              |
| `Agents.Session(id)` / `Sessions(agent)` | Look one up, or list one agent's.                                                                                                                           |
| `Agents.CloseSession`                    | Close one: a turn still streaming tools is given the chance to finish first.                                                                                |
| `Agents.EvictIdle(agent, now)`           | Evict by the agent's `limits.idle`. Explicit, not a goroutine, because eviction is an observable effect — and it skips a running session however long it has run. |
| `Agents.Add` / `Remove` / `Reload`       | Install, retract, swap. `Remove` refuses while sessions are open (`ErrSessionsActive`).                                                                     |
| `Agents.Close(ctx)`                      | Close the whole table, then let `Resource.Close` release the process.                                                                                       |

`limits` sits beside the grant in the agent document rather than inside it, because
it is the session table's policy and not a permission: a session does not limit
itself, and the digest of a grant never sees it.

## Ownership

One rule: **whoever built a value closes it.**

| Value                     | Closed by                                                  |
| ------------------------- | ---------------------------------------------------------- |
| `host.Resource`           | `Resource.Close`, in reverse build order, joining failures |
| `host.Agents`             | `Agents.Close`                                             |
| a session's cell and tree | the session, after its turn ends                           |
| a driver                  | the session that opened the program's driver               |
| a deployment's program    | `Agents`, when its last session is gone                    |
| the store                 | the application that opened it                             |

A session that outlives its runner writes into a corpse, which is why a close
cancels the turn and waits for it before releasing the driver and the boundary.

## What is not here

| Not in the process                  | Where it lives                                                  |
| ----------------------------------- | --------------------------------------------------------------- |
| Conversation history                | the checkpoint's event log — see [checkpoint.md](checkpoint.md) |
| The prompt, and the loop            | the deployment's program: `agent/graph`, `agent/a2a`            |
| The document formats                | `core/deploy` — see [deploy.md](deploy.md)                      |
| Authority decisions                 | `core/policy`                                                   |
| A correlation package               | `core/identity`, written only by `identity/inject`              |
| A per-agent runtime or record level | nothing: both are the process's                                 |
| Path resolution inside the platform | nothing: a root must be absolute and already exist              |
