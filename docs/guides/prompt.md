---
layout: default
title: Prompt Lifecycle Events
---
# Prompt Lifecycle Events

Prompts are how an engine asks the host (and through it, the end user)
for input mid-run. This guide covers the lifecycle events UI consumers
subscribe to.

## Prompt lifecycle events

Every prompt is announced with `PromptRequested` and terminated with
`PromptResolved`, both under the run's event namespace:

| Subject | Payload |
| --- | --- |
| `agent.run.<run_id>.prompt.requested` | `PromptRequested` |
| `agent.run.<run_id>.prompt.resolved` | `PromptResolved` |

Convenience subjects and patterns live in `core/runtime/session`:

```go
session.SubjectPromptRequested(runID)
session.SubjectPromptResolved(runID)
session.PatternPromptRequested() // agent.run.*.prompt.requested
session.PatternPromptResolved()  // agent.run.*.prompt.resolved
```

`PromptRequested` carries the run/turn/prompt IDs plus the
`agent.UserPrompt` payload.
`PromptResolved` is a best-effort notification carrying the same IDs
and a terminal `Status`:

- `replied` — a `Reply` satisfied the prompt;
- `expired` — the `AskUser` context ended before a reply arrived
  (timeout or host cancellation);
- `interrupted` — a turn interrupt closed the prompt;
- `closed` — the turn ended while the prompt was pending, or the
  prompt could not be published.

`PromptResolved` publish failures never change the `Reply` / `AskUser`
result. It lets a UI close or mark a pending interaction immediately
instead of waiting for the whole turn to finish.

### Subscribing through the Runtime

`Runtime.Attach` exposes the runtime's event router, so consumers do
not need to resolve the deployment document's `event_bus` resource:

```go
detach, err := app.Attach(ctx, session.PatternPromptRequested(),
	event.SinkFunc(func(ctx context.Context, env event.Envelope) error {
		var req session.PromptRequested
		if err := env.Decode(&req); err != nil {
			return err
		}
		// open the interaction view, keyed by req.PromptID
		return nil
	}))
if err != nil {
	return err
}
defer detach()
```

Subscribe to `PatternPromptResolved()` the same way and close the view
by `PromptID` when the matching `PromptResolved` arrives. External
attachments use the bus default backpressure (`DropNewest`), so a slow
UI drops envelopes instead of blocking the run pipeline; pass
`event.WithAttachBackpressure` to opt into a different policy.

Runtime agent lifecycle events (`runtime.agent.*`, `PatternAgentLifecycle`)
are published in a separate namespace — see
[runtime.md](runtime.md) for dynamic registration/removal.
