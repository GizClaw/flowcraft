---
layout: default
title: Hooks
---

# Hooks Guide

A hook is the host's policy at the boundaries of a turn. `core/agent` defines the
three points and runs two of them inside a turn; `core/host` runs `OnSession` when a
session opens and builds the chain; `core/deploy` lets a deployment declare its own.

The three points are the ones a **host** owns, rather than the ones an engine
owns: a session arrives, a turn arrives, a turn ends. They exist because a host
has no other place to reach a turn it did not start — a delegated child, a
message that came in over A2A — and because "what this agent is" belongs to the
agent, not to the substrate that happens to run it.

```go
type Hooks struct {
    // Runs after a session is opened or restored, before its turn.
    OnSession func(ctx context.Context, session SessionInfo) error
    // Runs before the turn reaches the driver and may rewrite its message.
    BeforeTurn func(ctx context.Context, turn *TurnInfo) error
    // Runs after the run ends, with the outcome it reached.
    AfterTurn func(ctx context.Context, turn TurnInfo, result TurnOutcome) error
}
```

## What a hook cannot do

Deliberately: it cannot rewrite the system framing, cut the engine's transcript,
or intercept one model call. Those are the engine's state, and a hook that
reached into them would turn `core/agent` into a prompt framework. A host that
needs per-call control owns the engine instead — see [graph.md](graph.md) for a
model node whose own code sits between the run and the provider.

## The three slots

| Slot         | Runs                                                   | Receives                                                    | An error means                                                                                      |
| ------------ | ------------------------------------------------------ | ----------------------------------------------------------- | --------------------------------------------------------------------------------------------------- |
| `OnSession`  | After a session is opened or restored, before its turn | `SessionInfo`: agent, session id, task id, state, `Resumed` | The open fails. A host that cannot prepare its memory refuses the turn instead of running it blind. |
| `BeforeTurn` | Before the turn reaches the driver                     | `*TurnInfo`: agent, session, message (mutable)              | The turn stops before it starts. Nothing was sent, so nothing has to be undone.                     |
| `AfterTurn`  | After the run ends                                     | `TurnInfo` and the `TurnOutcome` it reached                 | Logged, and the turn stands.                                                                        |

The `AfterTurn` asymmetry is not an oversight. The work is already durable, and
reporting an error after the terminal event would break the stream contract every
consumer relies on — a client that saw `Completed` cannot then be told the turn
failed.

`SessionInfo.Resumed` is what a memory hook branches on: a fresh task needs
recall, a restored one needs the state it left.

## Attaching hooks from Go

```go
hooks := agent.Hooks{
    BeforeTurn: func(ctx context.Context, turn *agent.TurnInfo) error {
        recap, err := memory.Recall(ctx, turn.Agent, turn.SessionID, turn.Message)
        if err != nil {
            return err
        }
        turn.Message = memory.Prefix(recap, turn.Message)
        return nil
    },
    AfterTurn: func(ctx context.Context, turn agent.TurnInfo, res agent.TurnOutcome) error {
        return memory.Persist(ctx, turn, res)
    },
}
```

`Turn.Message` is the input, metadata included, and `BeforeTurn` may replace the
whole value — that is the seam a policy uses to inject a recap without touching
the stored transcript. `TurnInfo.TaskID` is the task the turn continues: the host
mints it before the turn starts, so a hook sees it in both slots. `SessionInfo.TaskID`
is the one that depends on the session: empty for a new conversation, and the restored
task for a resumed one — which is what `Resumed` reports.

## Attaching hooks from a document

A deployment names hook implementations, and each one is registered in the process's
`deploy.Impls` table:

```yaml
agents:
  - name: forge
    hooks:
      - impl: memory
        settings:
          scope: session
      - impl: audit
        settings:
          sink: stdout
```

```go
impls.Hook(deploy.Impl("audit", func(cfg auditConfig) (agent.Hooks, error) {
    return auditHooks(cfg), nil
}))
```

`deploy.Impl` is the typed factory every implementation group uses: its `Plan` decodes
the written settings strictly, so a typo inside `settings` is a document error reported
with the rest of them, and its `Make` calls the constructor with the typed value. The
loader resolves a document's `hooks` into `[]deploy.PlannedHook` — the impl's name and a
`New` that produces `agent.Hooks` — and `host.Agents` builds them once, at install, so a
bad setting fails when the agent is deployed rather than on the first turn that reaches
a boundary.

An implementation a document names but the process never registered is a **document
error**, not `NotAvailable`: the table is the binary's build-time registration, exactly
like the providers and the program kinds. See [deploy.md](deploy.md).

Several hooks on one agent are chained **in declaration order**, and the chain is exactly
the document's list: the host contributes no hook of its own. `agent.Chain` composes
them — `OnSession` and `BeforeTurn` stop at the first failure, because a later hook would
be acting on a session or a message its predecessor rejected, and `AfterTurn` runs every
hook and logs what failed, because one policy's bookkeeping is not another's to cancel.

## Cost and blocking

Hooks run synchronously on the run's goroutine. A slow hook stalls the run and
its protocol stream, so a hook that does I/O belongs behind a queue of its own —
`host.WithRunSink` has the same rule for the same reason: the sink is called on the
turn's goroutine, after the terminal checkpoint is durable and before the after-turn
hooks, and it must be cheap and must not fail the turn.

## Delegation

A delegated child is a session of its own: the hooks run for it, with `Agent`
naming the child's deployment, not the caller's. A hook that keys on the session
identity therefore answers for the child — see [delegation.md](delegation.md).

## What this replaced

The previous `core/hooks` package ran `hooks.json` entries as `sh -c` children on
a large event vocabulary (`PreToolUse`, `PostToolUse`, `PermissionRequest`,
`TurnEnd`, …). It is gone: the vocabulary grew faster than anyone could keep
stable, and every command hook paid a process spawn on the turn's critical path.
The three in-process slots cover what hosts actually used those events for, and a
host that still wants external processes can spawn them from `AfterTurn`.
