---
layout: default
title: External Hooks
---
# External Hooks

`core/hooks` runs **external command hooks**: a `hooks.json` file maps
lifecycle event names to shell commands, and every matching command
receives one JSON event object on stdin. This guide covers the file
format, the event vocabulary, the invocation contract, and how
plugin-contributed hooks join the same runner.

There are two hook layers, and they do not mix:

| Layer | Declared in | Runs | Failure policy |
| --- | --- | --- | --- |
| `core/agent` slots (`prepare` / `observe` / `referee` / `commit`) | deployment document | in-process Go hooks, built from factories | may return an error and decide the turn |
| `core/hooks` (this guide) | `hooks.json` | `sh -c` child processes | never blocks: logged and skipped |

## hooks.json

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "^exec_command$",
        "hooks": [
          {"type": "command", "command": "audit-tool.sh", "timeout": 10}
        ]
      }
    ],
    "TurnEnd": [{"hooks": [{"command": "notify-send done"}]}]
  }
}
```

- `matcher` is a regex tested against the occurrence's **match value**
  — the first non-empty of the payload's `tool`, `source`, `reason`,
  `subagent` fields. Empty and `*` match every occurrence.
- `type` may be omitted or `command`; any other value is skipped, so a
  file written for a newer runner still loads here.
- `timeout` is seconds, `30` by default. A missing `command` or an
  unknown event name fails the load instead of being ignored.
- The primary file may be absent (a host with no hooks of its own still
  starts); a malformed primary file fails the build. A plugin source
  that cannot be loaded is skipped with a warning.

## Event vocabulary

The vocabulary is a platform ABI: frozen and append-only. Adding an
event is an interface change; renaming one is a breaking change.

| Event | Fired when | Payload beyond `event` |
| --- | --- | --- |
| `PreToolUse` | before a tool call runs | `tool`, `tool_input` |
| `PostToolUse` | after a tool call returns | `tool`, `tool_input`, `tool_result` |
| `UserPromptSubmit` | the user submits a prompt | `conversation_id`, `prompt` |
| `PermissionRequest` | an approval decision is requested | `tool`, `command`, `reason` |
| `TurnEnd` | a turn finishes | `conversation_id`, `run_id`, `status`, `error`, `usage` |
| `SessionStart` | a session begins | `source` |
| `SessionEnd` | a session ends | `source`, `reason` |
| `SubagentStart` | a delegated card is claimed | `subagent`, `card_id`, `run_id`, `status`, `target`, `message` |
| `SubagentStop` | a delegated card reaches a terminal status | same as `SubagentStart` |

The fields above are what the platform's own producers send and what the
matchers select on; a producer is free to add fields of its own, and a
hook that wants new ones has to tolerate their absence in older hosts.

`SessionStart` / `SessionEnd` are part of the vocabulary but have no
producer inside the platform: an application that owns a session
lifecycle fires them, and the event names are already reserved.

## The invocation contract

- One JSON object per invocation on stdin, always carrying `event` —
  stamped by the runner, so a producer cannot mislabel its event.
- The command runs under `sh -c`, in the source's directory when it has
  one.
- The timeout bounds the whole process **group**: expiry SIGKILLs the
  shell together with the descendants it spawned. On Windows a process
  cannot be killed as a group, so descendants may outlive the timeout —
  the one documented difference.
- The first 64 KiB of combined output are kept for the log line;
  further output is accepted and dropped, never buffered without bound.
- Failure never propagates. A hook that times out, exits non-zero, or
  cannot start is logged and skipped: `Manager.Fire` returns no error,
  because user hooks are advisory and an agent loop must not stop
  because a script broke.

## Sources and trust

A runner is built from one primary file plus any number of extra
sources:

```go
manager, err := hooks.LoadWithSources(ctx, "/etc/app/hooks.json",
    []hooks.ExtraSource{{
        Path:    "/plugins/hello/hooks.json",
        Dir:     "/plugins/hello",
        Trusted: false, // plugin content is third-party input
    }})
```

`Trusted: false` (the zero value) strips the content-bearing fields
before the command sees them — `tool_input`, `tool_result`, `prompt`,
`command`, `error`, `message`, `target` — leaving identifiers and
status fields. A trusted source sees the payload as-is. `craft` marks
every plugin source untrusted, which is how `hooks:provide` is honored
at execution time rather than only at contribution time.

## Producers

One runner, many producers: whoever owns an event's semantics fires it.

- Core-owned events ship with an adapter. The delegation kanban board
  fires `SubagentStart` / `SubagentStop` through `hooks.Observer`, which
  subscribes to `kanban.PatternAll()`.
- Application-owned events (`PreToolUse` in a tool middleware,
  `PermissionRequest` in an approval gate, `UserPromptSubmit` /
  `TurnEnd` in the prompt loop, …) are fired by the application at its
  own call sites.
- A new hook point is a new event plus an adapter — never a second
  mechanism.

## Deployment wiring

Both pieces are ordinary resources:

```yaml
resources:
  event_bus:
    kind: event.Bus
    impl: memory

  hooks:
    kind: hooks.Runner
    impl: local
    settings:
      path: ~/.config/app/hooks.json
    deps:
      plugins: craft.pluginhost   # optional: plugin hook sources

  hooks_observer:
    kind: hooks.SubagentObserver
    impl: local
    deps:
      events: event_bus
      runner: hooks
```

The runner value is a `*hooks.Manager`; the observer subscribes in the
wiring phase and stops in `Close`. Because a craft host injects its
plugin host as the `craft.pluginhost` external, a runner declares the
dependency contract once:

```go
factory := hooks.NewFactory(
    hooks.WithSourceDep("plugins", "craft.PluginHost"))
```

A host that wires the provider in Go instead uses
`hooks.WithSources(provider)` — the provider (craft's `plugin.Host`)
implements `HookSources() []hooks.ExtraSource` and is asked at every
build, so enabling or disabling a plugin is visible on the next reload.
