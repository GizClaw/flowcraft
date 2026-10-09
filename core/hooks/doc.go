// Package hooks runs external command hooks: a hooks.json file maps
// lifecycle event names to shell commands, and every matching command
// receives one JSON event object on stdin.
//
// # Transport contract
//
// The contract between the platform and a hook command is an ABI:
//
//   - The event vocabulary is frozen and append-only: PreToolUse,
//     PostToolUse, UserPromptSubmit, PermissionRequest, TurnEnd,
//     SessionStart, SessionEnd, SubagentStart, SubagentStop. Adding an
//     event is an interface change, renaming one is a breaking change.
//   - Every invocation receives exactly one JSON object on stdin. It
//     always carries "event", stamped by the runner rather than trusted
//     from the producer, plus the event-specific fields the producer
//     supplies: "tool" / "tool_input" / "tool_result" for tool events,
//     "prompt" and "source" for prompt and session events, "subagent" /
//     "card_id" / "run_id" / "status" for subagent events.
//   - A command runs under sh -c, in the source's directory when it has
//     one, with the hook's timeout (30s by default) bounding it. On
//     expiry the whole process group is killed, so a hook that spawned
//     descendants does not leave them behind.
//   - The first 64 KiB of combined output are kept, and logged.
//   - Failure never propagates. A hook that times out, exits non-zero,
//     or cannot even start is logged and skipped: user hooks are
//     advisory and an agent loop must not stop because a script broke.
//     [Manager.Fire] returns no error for that reason.
//
// # Producers
//
// One runner, many producers: whoever owns an event's semantics fires
// it. Core-owned events ship with an adapter in this package — the
// delegation kanban board fires SubagentStart and SubagentStop through
// [Observer]. Application-owned events (tool use, prompt submission,
// turn end, session boundaries) are fired by the application at its own
// call sites. A new hook point is a new event plus an adapter, never a
// second mechanism.
//
// # Sources and trust
//
// A source is the user's hooks.json (loaded first, trusted), an
// application-injected [ExtraSource], or a plugin-contributed file
// (loaded untrusted: the runner strips the content-bearing fields —
// tool inputs, tool results, prompts, commands, errors, messages,
// targets — before the command runs, since plugin content is
// third-party input). A missing primary file yields a runner with no
// groups; a source that fails to load is skipped with a warning, so one
// broken plugin cannot disarm every hook in a deployment.
//
// # Relationship to the core/agent lifecycle slots
//
// core/agent declares four in-process lifecycle slots — prepare,
// observe, referee, commit — whose hooks are Go factories registered at
// build time. Those participate in a run: they return errors and may
// block, revise, or discard a turn. This package is the other layer:
// external commands loaded from a file, which must never block or break
// a run. The two are deliberately not merged. A failure policy of
// "never propagate" and one of "decide the run" are opposites, and the
// process-internal slots are a contract between libraries while this is
// an ABI for scripts.
package hooks
