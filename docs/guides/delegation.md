---
layout: default
title: Delegation
---
# Delegation Guide

`core/delegation` lets one agent hand work to another and get an answer back. A
`Service` binds a policy to the host's session table: the targets it may reach, the
modes it may use, how deep a chain may go, how many may run at once, and how large
one input may be.

```go
service, err := delegation.New(
    sessions,                      // the host's session table (agent-host implements it)
    delegation.Policy{/* from the deployment */},
    delegation.WithStore(delegation.NewMemStore()),   // async jobs
)
source := delegation.Tools(service)                   // a tool.Source
```

There is no directory, no session provider and no backend resource. The session
table **is** discovery: `Sessions.Names()` is who can be reached, and
`Sessions.Session(name, id)` is how one is entered.

## Deployment

Delegation is a block on an agent, not an assembly graph:

```yaml
agents:
  - name: forge
    delegation:
      targets: [forge-review, forge-test]   # empty = every deployed agent except self
      modes: [sync, async]                  # empty = sync only
      max_depth: 4                          # zero = 4
      max_concurrent: 4                     # zero = 4
      timeout: 5m                           # zero = the caller's context
      max_input_bytes: 65536                # zero = 64 KiB
```

An **absent** block means the agent gets no delegation tools at all — not
"unrestricted". `deploy` turns the block into a `delegation.Policy`, and
`agent-host` builds the service per deployment, so every generation delegates
against its own agents.

`delegation.Tools(service)` is the model-facing surface:

| Tool | Use |
| --- | --- |
| `delegate` | Hand an input to a target; sync waits, async returns a delegation id. |
| `delegation_targets` | What this agent may reach, from the policy. |
| `delegation_status` | The state of one delegation, by id, including a question it asked. |
| `delegation_answer` | Answer a child that is waiting for input. |
| `delegation_cancel` | Stop a child. |

The tools read the policy on every call, so a projection rebuilt for a new
generation sees that generation's targets.

## One delegation, step by step

`Service.Delegate` is deliberately strict about its preconditions, because each
one is a fact that would otherwise become a wrong answer somewhere else:

1. **A parent session** — `runctx.From(ctx)` must carry one. A delegation outside
   a run has no place to return to.
2. **A tool call id** — `tool.CallIDFromContext`. It is what makes the child
   session key deterministic.
3. **The mode is allowed** by the policy, and the target is in `targets` (or
   `targets` is empty and the target is not the caller).
4. **The input is non-empty** and under `max_input_bytes`.
5. **The depth is under `max_depth`**; a chain that reaches it is refused rather
   than truncated.

The child session key is derived, not random:

```text
<parent session>/d/<target>/<tool call id>
```

That is the whole idempotency story. A tool call that is retried — a dropped
connection, a client that resubmits — lands on the **same** child session, and a
child that already reached a terminal state is reported from its stored state
instead of being run again. A child that is still `working` is a `Conflict`, not a
second run behind the user's back.

Sync mode then runs the child and collects its answer; async mode stores a `Job`
and returns as soon as the work is accepted.

## Statuses and outcomes

| Status | Meaning |
| --- | --- |
| `accepted` | An async job is stored and not yet claimed. |
| `running` | The child is working. |
| `completed` | The child produced a final answer. |
| `input_required` | The child asked a question; answer it with `delegation_answer`. |
| `failed` | The child failed or was rejected. |
| `canceled` | The child was stopped. |

`Outcome` carries the status, the delegation id, the target, the child session id,
the content or the question, an error string, and the lineage:

| Field | Meaning |
| --- | --- |
| `Depth` | Edges above this run; the root sees zero. |
| `ChainID` | The root delegation of the chain, `sha256(session, task, call)`; every descendant keeps it, so a chain is one trace. |

## Async jobs

An async delegation is a `Job` in a `Store`:

```go
type Store interface {
    Create(ctx context.Context, job Job) (created Job, found bool, err error)
    Get(ctx context.Context, id string) (Job, error)
    Update(ctx context.Context, id string, mutate func(*Job)) error
}
```

`Create` returns `found` when a job with that id already exists — the same
derived-key rule one layer down, so a retried submission does not create a second
job. `Job` carries the parent identity (session, agent, task, call id), the
traceparent, the child session id, the content or question, and the timestamps.
`delegation.MemStore` is the reference implementation; a durable store is a table
with the same three methods.

## Lineage

```go
call, ok := delegation.CallFrom(ctx)     // {Depth, ChainID}
next := delegation.WithCall(ctx, call.Child())
depth := delegation.DepthFromContext(ctx)
```

`Call` is a value, not a set of context keys, so a later field (a budget, an eval
marker) does not invent another key. The service stamps it before running the
child, and everything downstream — a hook, a child delegation — reads the same
lineage.

## What the child inherits, and what it does not

The child is **a session of its own**: its own task, its own checkpoint, its own
hooks (with `Agent` naming the child's deployment), and its own sandbox
configuration.

Core derives no policy for it. In particular, a deployment that confines or gates
work per session has to decide the child's boundary explicitly — `delegation.Policy`
bounds delegation (who, how many, how deep, how large) and says nothing about the
child's write boundary or approval mode. The child's own `sandbox` block, and a
hook that recognises the child, are where that decision lives.

Budget: the caller's context bounds everything, `timeout` bounds one delegation,
and `max_concurrent` bounds in-flight sync children with a slot table — a child
beyond the limit waits, and a cancelled caller does not hold a slot.

## What this replaced

`delegation.Directory` (a resource that owned discovery), `delegation.SessionProvider`
(a resource that minted child identities), an `async` backend resource, escrow
refs and `RunIDNotifier`. All of it folded into three things: the host's session
table for discovery, a derived session key for identity, and a `Store` for async
jobs. The behaviour that mattered — a retry must not re-run a completed child, a
chain must be traceable, a child's boundary must not be guessed — is unchanged and
now stated in one place.
