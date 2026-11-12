---
layout: default
title: Script Rules
---
# Script Rules

`core/agent-graph/nodes/script` binds a rule script to a graph node. A rule reads
the state it declares, returns the state it declares, and does nothing else: it
does not speak (only a model node streams text), it does not open a session, and
it cannot suspend.

That is the whole reason it can be bounded, tested and replayed. It is also what
makes it *not* the script runtime this repository removed: there is no board, no
channel and no publish — the graph's state already is the board.

## Configuration

```yaml
- id: tally
  kind: script
  timeout: 2s                       # the engine bounds one invocation
  config:
    backend: starlark               # resolved against the host's backend table
    path: rules/vote_tally.star     # handed to the source
    reads:  [game, public_log]      # projected into the script
    writes: [game, public_log]      # the only keys it may return
    allow:  [tool:werewolf_game_event]   # empty means it can call nothing
    max_output_bytes: 65536         # over the cap fails, never truncates
    budget:
      max_steps: 200000             # reproducible: same source, same state, same steps
      memory_mib: 64                # only a backend that declares it may be asked
    version: pinned                 # pinned | latest
```

A configuration states what it needs, and construction fails when the backend
cannot honour it:

| configuration | requires |
| --- | --- |
| `budget.max_steps` | `Capabilities.Terminating` |
| `budget.memory_mib` | `Capabilities.MemoryBounded` |
| `allow` | `Capabilities.Tools`, and a runtime to dispatch through |

That is deliberate: a document must never run with less than it asked for.

## The script

```python
def run(state):
    game = state["game"]          # only the keys in config.reads
    votes = {}
    for vote in game["votes"]:
        votes[vote] = votes.get(vote, 0) + 1
    return {"game": game, "votes": votes}   # only the keys in config.writes
```

- The entry point is `run(state)`; top-level code runs on every invocation, which
  is what keeps one call from observing another's globals. Put logic in `run()`.
- `state` holds the `reads` keys as data: `text` becomes a string, `json` a dict,
  `bool` a bool, and `messages` a sequence of `{"role", "text"}` objects.
  (On the Go side a transcript is handed to the backend as `script.Messages`: the
  sequence itself, so a backend builds its own representation instead of paying
  for an intermediate map per message.)
- A returned key must be declared in `writes`, and its value must match the kind
  the graph declared for it. `messages` is a **delta**: the key merges with
  append, so returning the transcript would duplicate it.
- Host functions: `log(*args)` and, when the config allows tools,
  `tool(name, args)`.

### Transcripts are a view, not a list

`state["history"]` behaves like a list of `{"role", "text"}` dicts — `len()`,
`t[i]` (negative indices included), `t[a:b]` and `for message in t` all work — but
it is answered lazily:

| question | cost |
| --- | --- |
| `len(t)` / `t.count` | O(1) |
| `t.last` / `t.first` | one dict |
| `t[i]`, `t[-2:]` | the messages asked for |
| `for message in t` | every message, which is what asking for them means |
| `str(t)` | every message, because that is what printing asks |

The point is the shape a rule usually has: "how many turns have we had" and
"what did they just say" cost nothing, and a 5,000-message transcript is the same
price as an empty one. A rule that wants the whole history as text pays for it,
once, when it asks.

The view is read-only: `t.pop()`, `t.append(...)` and `t[0] = ...` are errors. A
rule computes new state and returns it; it does not edit the transcript it was
given.

## Tools

```python
def run(state):
    tool("werewolf_game_event", {"event_type": "night_resolve", "phase": "dawn"})
    return {"phase": "dawn"}
```

`tool` returns `{"ok": True, "text": ...}` and refuses anything `allow` does not
name. The call goes through the runtime's ordinary path — the same middleware
chain, policy checks and catalog view a `tool` node uses — and each call is
recorded with its argument hash, so a replay answers from the record instead of
running the tool again.

A failed tool aborts the script. Starlark has no exception to catch, and a rule
that cannot complete its action should fail the step rather than guess.

## Versions and replay

Every invocation records `script.run` (source reference and version) and every
call records `script.tool`, both of them durably with the step.

- `version: latest` (default) takes whatever the source holds now.
- `version: pinned` refuses to continue when the source changed since the task
  recorded its version: a fix lands on new games, not in the middle of one.
- During a replay (`graph.RunnerConfig.Replay`) a tool call is answered from its
  record. A miss fails loudly, and nothing is recorded again.

## Budgets and non-goals

- **Steps** are the primary bound because they are reproducible: the same source
  and state take the same number of steps on every machine.
- The caller's context and the node's `timeout` bound the wall clock.
- **Memory is not bounded by the node.** A step budget bounds work, not
  allocation. A deployment that needs a hard cap must run the agent under its own
  limit (cgroup, rlimit, container), or choose a backend that isolates the script.

## Backends

| backend | deterministic | terminating | memory bound | isolated | tools | cost |
| --- | --- | --- | --- | --- | --- | --- |
| `starlark` | ✓ language | ✓ language | ✗ | ✗ | ✓ | ~6 µs per rule |

A backend is a language: `Compile(ctx, Script) (Program, error)` and
`Run(ctx, Program, state, budget, host)`. To add one:

1. implement `script.Backend`, declaring only what you can promise;
2. run `scripttest.Run(t, scripttest.Fixture{...})` — the same contract cases
   every backend runs, so a second language is verified rather than re-derived;
3. keep `Host` as the only way out: tool policy, recording and replay live in the
   node, so no backend can get them wrong.

## Errors

| failure | class |
| --- | --- |
| bad config, missing source, compile error | `Validation` |
| a script that fails while running | `Aborted` |
| a budget that runs out | `BudgetExceeded` |
| a write the config does not declare, or the wrong kind | `Validation` |
| a tool outside `allow` | `Forbidden` |
