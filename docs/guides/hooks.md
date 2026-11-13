---
layout: default
title: Hooks
---
# Hooks Guide

A hook is the host's policy at the boundaries of a turn. `core/agent` defines the
three points; `core/agent-host` runs them; `core/deploy` lets a deployment declare
its own.

The three points are the ones a **host** owns, rather than the ones an engine
owns: a session arrives, a turn arrives, a turn ends. They exist because a host
has no other place to reach a turn it did not start — a delegated child, a
message that came in over A2A — and because "what this agent is" belongs to the
agent, not to the substrate that happens to run it.

```go
type Hooks struct {
    // Runs after a session is opened or restored, before its turn.
    OnSession func(ctx context.Context, session SessionInfo) error
    // Runs before the turn reaches the engine and may rewrite its message.
    BeforeTurn func(ctx context.Context, turn *Turn) error
    // Runs after the run ends, with the state and result it reached.
    AfterTurn func(ctx context.Context, turn Turn, result Result) error
}
```

## What a hook cannot do

Deliberately: it cannot rewrite the system framing, cut the engine's transcript,
or intercept one model call. Those are the engine's state, and a hook that
reached into them would turn `core/agent` into a prompt framework. A host that
needs per-call control owns the engine instead — see [graph.md](graph.md) for a
model node whose own code sits between the run and the provider.

## The three slots

| Slot | Runs | Receives | An error means |
| --- | --- | --- | --- |
| `OnSession` | After a session is opened or restored, before its turn | `SessionInfo`: agent, session id, task id, state, `Resumed` | The open fails. A host that cannot prepare its memory refuses the turn instead of running it blind. |
| `BeforeTurn` | Before the turn reaches the engine | `*Turn`: agent, session, message, attributes | The turn stops before it starts. Nothing was sent, so nothing has to be undone. |
| `AfterTurn` | After the run ends | `Turn` and the `Result` it reached | Logged, and the turn stands. |

The `AfterTurn` asymmetry is not an oversight. The work is already durable, and
reporting an error after the terminal event would break the stream contract every
consumer relies on — a client that saw `Completed` cannot then be told the turn
failed.

`SessionInfo.Resumed` is what a memory hook branches on: a fresh task needs
recall, a restored one needs the state it left.

## Attaching hooks from Go

```go
hooks := agent.Hooks{
    BeforeTurn: func(ctx context.Context, turn *agent.Turn) error {
        recap, err := memory.Recall(ctx, turn.Agent, turn.SessionID, turn.Message)
        if err != nil {
            return err
        }
        turn.Message = memory.Prefix(recap, turn.Message)
        return nil
    },
    AfterTurn: func(ctx context.Context, turn agent.Turn, res agent.Result) error {
        return memory.Persist(ctx, turn, res)
    },
}
```

`Turn.Attributes` carries what the deployment declared for this run, and is where
a policy that is keyed by attributes (rather than by session) reads its input.

## Attaching hooks from a document

A deployment names hook kinds, and each kind is registered by the host:

```yaml
agents:
  - name: forge
    hooks:
      - kind: memory
        settings:
          scope: session
      - kind: audit
        settings:
          sink: stdout
```

```go
kinds := deploy.BuiltinKinds().WithHooks(deploy.HookKind{
    Name: "audit",
    Decode: func(settings json.RawMessage) (any, error) {
        var cfg auditConfig
        return cfg, decode.Strict(settings, &cfg)   // a typo fails the load
    },
    Build: func(ctx context.Context, deps deploy.HookDeps) (agent.Hooks, error) {
        cfg := deps.Spec.Settings           // decode again for the typed value
        return auditHooks(deps.Agent, cfg), nil
    },
})
```

| `HookDeps` field | What it gives the kind |
| --- | --- |
| `Agent` | The deployment name the hook is attached to — what a seat-bound policy keys itself by. |
| `Spec` | The kind and its raw settings. |
| `Runtime` | The agent's substrate; nil for a remote engine, because a hook that only observes turns does not need one. |
| `Store` | The checkpoint store, so a hook can read the task log to assemble what it needs. |
| `LoadOptions` | Placeholder resolvers, for a kind that reads its own documents. |

`Decode` runs at load (so a typo fails before the agent is built) and again inside
`Build` for the typed value — the same two-phase contract engine and protocol
kinds use. See [deploy.md](deploy.md).

Several hooks on one agent are chained **in declaration order**. The host's own
`Bindings.Hooks` runs before them, so a host can always observe or adjust what a
deployment configured. `BuiltinKinds()` registers no hook kinds: a host's policy
is the host's.

## Cost and blocking

Hooks run synchronously on the run's goroutine. A slow hook stalls the run and
its protocol stream, so a hook that does I/O belongs behind a queue of its own —
`OnEvent` in `agent-host.Bindings` has the same rule for the same reason.

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
