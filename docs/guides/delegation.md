---
layout: default
title: Delegation
---

# Delegation Guide

Delegation is a permission, not a service. `policy.Delegation`, a block on the grant a
session is opened with, says which agents it may reach, in which modes, how deep and
how large an input may be. A session whose grant carries a non-nil `Delegation` is
given one capability — `platform.DelegationSlot`, filled by the host — and
`tool/builtin/delegation` exposes the four tools that read it:

```text
Grant.Delegation (policy.Delegation)
    → host fills platform.DelegationSlot with *host.delegator
    → tool/builtin/delegation reads it from tool.Binding.Caps, per call
```

There is no directory and no session provider: `targets` **is** discovery, and
`host.Agents.Open` is how a child is entered. A tool reaches the capability through its
`tool.Binding`, so nothing in `core` imports a tool module and nothing in a tool module
imports the host.

## Deployment

Delegation is a block on the authority a session is opened with. No document can grant
it, and an absent block is not "unrestricted" — it is no delegation at all, which is the
safe zero value for the one capability that can manufacture a run under a different
authority:

```go
authority := policy.Grant{
    // …
    Delegation: &policy.Delegation{
        Targets:       []string{"forge-review", "forge-test"}, // required: naming is how it is granted
        Modes:         []policy.DelegationMode{policy.DelegationSync, policy.DelegationAsync},
        MaxDepth:      4,     // zero caps nothing
        MaxConcurrent: 4,     // zero is unbounded; the session's own limit still applies
        Timeout:       policy.Duration(5 * time.Minute), // zero leaves it to the caller's context
        MaxInputBytes: 65536, // zero uses policy.DefaultMaxInputBytes (64 KiB)
    },
}
```

The child's authority is resolved from the trusted side. With no resolver installed it
inherits the parent's grant, which is the one rule that cannot escalate; an application
that needs per-target authority installs `host.WithDelegationGrant` and returns something
narrower.

| Field             | Rule                                                                                                                         |
| ----------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| `targets`         | Required and non-empty. There is no "every deployed agent" spelling, so a target the process does not carry fails at `Open`. |
| `modes`           | Required and non-empty; each entry is `sync` or `async`.                                                                     |
| `max_depth`       | Caps the edges above a leaf. Zero caps nothing, so a grant that wants a limit states one.                                    |
| `max_concurrent`  | Caps in-flight synchronous edges from one session, as a slot table. Zero is unbounded.                                       |
| `timeout`         | Bounds one synchronous edge. Zero leaves the child to the caller's context, which for a sync edge is the parent's turn.      |
| `max_input_bytes` | Caps the text one delegation hands over. Zero uses `policy.DefaultMaxInputBytes`.                                            |

The values are validated with the grant (`policy.Delegation.Validate`, reached through
`Grant.Normalized`), so a block with no target or no mode is refused at open rather than
being a session that silently cannot delegate. Every edge is admitted against the **parent's**
authority: only the caller's side can answer "may this session hand work to that agent, that
way, this deep". The child then runs under its own — inherited from the parent, or resolved by
the application — never under authority borrowed from somewhere the parent could not reach.

## The four verbs

`platform.Delegation` is the seam. It has exactly four verbs, and the host's
implementation of each is what a tool call reaches:

| Verb       | Tool                | Use                                                                                             |
| ---------- | ------------------- | ----------------------------------------------------------------------------------------------- |
| `Delegate` | `delegate`          | Run one target: `{target, input, mode}`. Sync waits for the answer, async hands off.            |
| `Status`   | `delegation_status` | Where a child stands, by the session id `Delegate` returned.                                    |
| `Answer`   | `delegation_answer` | Continue a child that paused: a decision for an approval, text for anything else.               |
| `Cancel`   | `delegation_cancel` | Stop a child, including one paused for an answer. A child that is already gone is not an error. |

There is no steer verb. `delegation_answer` resumes a child that stopped and asked a
question — it lands on `Agents.Resume`, the only way out of `input-required`; feeding a _running_ turn is `host.Agents.Steer`, which the host owns and a
delegated child is not addressed by.

`platform.DelegationOutcome` is what the tools report back — the child session, its
state, its answer or its question, and the lineage:

| Field      | Meaning                                                                                                 |
| ---------- | ------------------------------------------------------------------------------------------------------- |
| `Status`   | The child's state: `submitted` right after an async hand-off; otherwise the state its events folded to. |
| `Session`  | The child session id `Delegate` created — the address every follow-up verb takes.                       |
| `Content`  | The child's answer, folded from its event stream.                                                       |
| `Question` | What the child asked when it paused with `input_required`.                                              |
| `Chain`    | The root delegation this run belongs to.                                                                |
| `Depth`    | The delegation edges above the child. A caller's own run is depth 0.                                    |

## One delegation, step by step

`Delegate` is strict about its preconditions, because each one is a fact that would
otherwise become a wrong answer somewhere else:

1. **A parent session with a policy.** The parent's grant carries a `Delegation`, and
   the target is in `targets`. A session that may not delegate has no capability at all,
   so the tool is refused before the call.
2. **The mode is allowed** by the same grant.
3. **The chain is under `max_depth`**; an edge that would exceed it is refused rather
   than truncated.
4. **The input fits** under the policy's `InputLimit()`.

The child session id is derived from the parent's, never random:

```text
<parent session>/<target>-<edge>
```

`edge` is a per-process counter in base 36 plus four random bytes. The counter keeps
the ids of one chain readable and ordered; the suffix is what makes an id unique
across processes, because the counter restarts with the process and a restarted
parent would otherwise mint the id of a child whose task is still in the store —
continuing that child's conversation instead of starting one. Nothing dedupes two
`Delegate` calls for the same input, because each call is a new child.

The host opens the child with the lineage it just minted, and the first edge of a chain
**mints** its id (`chain-` and eight random bytes); every edge below it keeps that id
and adds one to the depth. Without an id the lineage cannot be attached at all, which is
why the host mints one before opening.

**Sync** runs the child on the caller's context, bounded by `max_concurrent` (a slot the
parent holds for the run) and by `timeout`; the child session exists for the length of
the call and is closed when it returns — unless it paused, see [Approvals](#approvals).
The answer, the question and the terminal state
are folded from the child's event stream into the outcome. A child that outlives
`timeout` is cancelled rather than abandoned, and the caller sees `DeadlineExceeded`.

**Async** starts the child on a goroutine of its own and returns at once with
`Status: submitted` and the session id. The run outlives the turn that asked for it, so
it gets a root span of its own linked to the asking turn — a child of a span that has
already ended would be a trace nobody can read — and the goroutine closes the session
when the run ends, unless the child paused to ask.

The follow-up verbs address the child by session id: `Status` reads the state its
checkpoint recorded (a host with no `agent.Store` refuses it with `NotAvailable`),
`Answer` runs the child again on the caller's context and folds what it produced next,
and `Cancel` stops it. `Answer` and `Cancel` go through the session table, so they reach
a child this process still holds open; one it has closed is `NotAvailable` for
`Answer` and `NotFound` for `Cancel`. A session may only address the edges it opened, and
their descendants: the check is a prefix test on the id, so a caller cannot reach
another conversation's child, or its own parent.

## Lineage

- The lineage is the host's to attach, not the caller's to state: delegation is the only
  code path that opens a child, and it is the only place that knows the parent, the
  chain id and the depth. There is no `Open` option for it, because a chain a caller
  supplied would be a delegation edge that never happened — and three things read it:
  `max_depth` counts its depth, the approval route asks its last element for a decision,
  and the spans name it.
- The lineage lives on `identity.Run` (`ChainID`, `Depth`) and is attached per run by the
  session, after the session facts, through `inject.Chain`. Every span a layer beneath
  opens carries it, so a whole chain is one trace.
- Both fields are zero for a run that was never delegated, and `Run.Pairs` omits what is
  unset rather than rendering an empty attribute.
- `Chain.Lineage` lists the session ids above the child, root first and the immediate parent
  last. A decision that has to go back up the line — a subagent asking the agent that
  delegated to it — needs the ids; the child id encodes its parent, but a prefix is a naming
  accident rather than a contract.

## Delivery: how a hand-off reports back

A synchronous edge returns the child's answer as the tool result, so there is nothing to
deliver. An **asynchronous** edge is a hand-off: the caller is gone when the child finishes,
and the result reaches the parent through the first channel that can take it.

| Channel             | When                                                                                     | Who reads the events                                                 |
| ------------------- | ---------------------------------------------------------------------------------------- | -------------------------------------------------------------------- |
| **Steer**           | the parent has a turn in flight and its driver can take input                            | the caller already consuming that turn's iterator — no second source |
| **A host turn**     | the parent is idle, or its driver cannot be steered but can take a message between turns | `host.WithHostTurnEvents`; without it the record only                |
| **The application** | the parent is gone, or its program cannot take a message between turns                   | `host.WithDelegationDelivery`                                        |

```go
agents := host.NewAgents(res,
    host.WithHostTurnEvents(func(ctx context.Context, sessionID string, kind host.HostTurnKind, ev agent.Event, err error) bool {
        // kind is host.HostTurnDelivery or host.HostTurnDelegation
        render(sessionID, ev)
        return true
    }),
    host.WithDelegationDelivery(func(ctx context.Context, sessionID string, msg message.Message) bool {
        // nothing could put this into the conversation; queue it, persist it, or drop it
        return true
    }),
)
```

Rules that make the three channels predictable:

- **One event, one consumer.** The sink is called only for turns the host started itself;
  a turn the application began through `Agents.Run` is read from its own iterator and the
  sink sees nothing of it.
- **The sink must not block and must not call `Run` for the same session.** It runs on the
  goroutine driving the turn, and different sessions may deliver concurrently.
- **A steer is only delivered once it is applied.** If the turn ends before a boundary, the
  host re-offers the message as a turn of its own.
- **A program that cannot take a message between turns cannot be delivered to.** A graph
  needs an `input` state slot; a driver reports it through `agent.InputSink`.
- **A delivery turn and a caller's `Run` never overlap.** One session has one turn. A delivery
  waits for a caller's turn to finish: taking the slot is one decision, made under the session's
  lock, and a waiting delivery re-checks and waits again. A caller that arrives during a delivery
  gets a conflict naming it, and retrying is the answer.

The delivered message is a **user turn** — the conversation gained a message, and the
platform does not fabricate an assistant turn the model never wrote. Its text names the
agent, the child session, the status and the answer; its metadata carries the same facts
for an application that must not parse a sentence:

| Attribute            | Value                                             |
| -------------------- | ------------------------------------------------- |
| `delegation.session` | the child session id                              |
| `delegation.agent`   | the deployment the child ran                      |
| `delegation.chain`   | the delegation chain the run belongs to           |
| `delegation.depth`   | how many edges above the child                    |
| `delegation.status`  | the child's task state when the delivery was made |

## Approvals

A child that runs a command its deployment does not whitelist asks **its parent** before
it runs, not a person. The agent that delegated the work holds the conversation that
made the command make sense; a prompt to an operator would throw that away and ask
about `git push` with no idea what is being pushed.

The question is a pre-flight, not a retry. When the child's tool node is about to run a
batch it first asks each call's tool whether it needs an answer (`tool.Asker`, reached
through the dispatcher's `Ask`). A question suspends the child's step **before anything
executes**, so the command does not run twice when the child resumes; the turn ends with
`input-required` and the question reaches the parent — as the tool result of a sync
`delegate`, or as a delivered message for an async one. The child session stays open.

The parent answers with `delegation_answer`:

| `decision` | What happens                                                                           |
| ---------- | -------------------------------------------------------------------------------------- |
| `approve`  | The command runs when the child retries it.                                            |
| `deny`     | It does not run, and the refusal is final: the child is told not to retry or escalate. |
| `escalate` | The deployment's `Approver` — a person or a service — decides; there is no one else.   |

An answer is bound to one command and one directory (a digest of the argv and the
workdir) for fifteen minutes. An approval of `git status` is therefore not an approval
of `git push`, the same command in another checkout is another question, and a decision
nobody used does not stay in force for the rest of the session.

A session a caller opened has no parent, so it keeps the old behaviour:
`Approval.Approver` is asked on the turn's goroutine. A refusal from either side is a
`policy.Denial` with `CheckApproval`, which is what a model reads to correct itself and
what a dashboard counts. A delegation the parent's grant does not permit is a denial
with `CheckDelegation`.

## What the child inherits, and what it does not

The child is **a session of its own**: its own task, its own checkpoint, its own hooks
and its own program (both from the target's deployment), and its own cell. Its
**authority** is inherited from the parent unless the application installed
`host.WithDelegationGrant`, which is what keeps delegation from manufacturing authority:
the default cannot escalate, and a resolver is trusted host code that narrows.

What crosses is the lineage and nothing else. The host runs the child on
`identity.Detach` plus `agent.WithoutFailureBox`: without the first, the child would
carry the parent's conversation, bind id and span attributes; without the second, the
parent's first-wins failure box would record the child's model error as the parent
turn's reason.

Core derives no authority for the child, and in particular no boundary: `Delegation`
bounds delegation (who, how many, how deep, how large) and says nothing about the
child's write boundary, network posture or approval mode. An application that confines
or gates work per target does that in its `WithDelegationGrant` resolver, or in a hook
that recognises the child.

## What this replaced

`core/delegation` is gone: its `Service` (a value that bound a policy to the host's
session table), its own `Policy`, an async `Job` in a `Store` with `MemStore`, and a
`Tools(service)` source. So is the run-identity carrier the lineage used to be stamped
into. What replaced each part is one existing thing: the policy is `Grant.Delegation` on
a `policy.Grant`, the verbs are `core/tool/platform`'s `Delegation` capability, the
tools are a `tool.Source` in the `tool` module, the child is a session of
`host.Agents`, and the lineage is `identity.Chain`. The behaviour that mattered — a
chain must be traceable, a child's boundary must not be guessed, a caller can only
address its own edges — is unchanged and now stated in one place.
