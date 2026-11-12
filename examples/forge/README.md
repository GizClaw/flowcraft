# Forge — a werewolf table on the FlowCraft stack

Forge is the runnable local demo for FlowCraft. It deploys a workspace directory
into a small cast of agents and plays a game of werewolf: one referee that owns the
rules, eight seats that speak, one of them played by you.

It is a host, not a framework: everything it does with the stack is what an
application has to write — open a workspace, deploy agents, drive a session, read
the events back out.

中文版见 [README_zh.md](README_zh.md)。

## Quickstart

Go 1.26+ and an OpenAI credential.

```bash
cd examples/forge
cp .env.example .env          # then set OPENAI_API_KEY=...
go run . help
go run . tui new              # pick "werewolf" and start the table
go run . test -test werewolf/opening
go run . test-auto --raid werewolf --rounds 8     # let it play itself
```

## Commands

| Command | What it does |
|---|---|
| `forge workspace create --config <raid> --workspace <dir>` | copy a raid into a workspace |
| `forge workspace inspect --workspace <dir>` | every agent: director or speaker, label, graph, models, tools, delegates, hooks |
| `forge inspect <workspace> [--task <id>] [--kind <kind>]` | one run's timeline, read back from the store |
| `forge config raid\|persona\|test list` | list scenarios and tests |
| `forge tui new` / `forge tui resume` | the interactive table |
| `forge test -test <raid>/<name> [--timeout 2m]` | one scripted test, written to `.out/<run>/` |
| `forge test-auto --raid <raid> [--player <agent>] [--rounds 24]` | play a whole game, the human seat answered by its own agent |

Scenarios resolve from `--scenarios`, `FORGE_SCENARIOS`, the executable's
directory, the working directory and the per-user config directory, in that order.

## The table: one graph, eight agents

A raid is a **C3** deployment: three kinds of participant, no more.

```text
referee      owns the rules and the table; makes no model call of its own
seat_1..8    one agent per seat, reached through the delegate tool
you          seat_3, answered by a person (or by seat_3's own agent in test-auto)
```

`deploy.yaml` is the cast:

```yaml
agents:
  - name: referee
    engine: {kind: graph, settings: {path: ${workspace:root}/graphs/referee.yaml}}
    delegation:                       # sync: a rule asks a seat and waits
      targets: [seat_1, seat_2, seat_3, seat_4, seat_5, seat_6, seat_7, seat_8]
      modes: [sync]
      timeout: 2m
  - name: seat_1
    engine: {kind: graph, settings: {path: ${workspace:root}/graphs/seat_1.yaml}}
    hooks:
      - kind: seat-memory             # the seat's own memory across turns
        settings: {dir: .forge/memory, keep: 12}
```

The referee's graph is the game: eleven script nodes (`seed`, `input`, `route`,
`night`, `dawn`, `day`, `vote`, `exile`, `hunter`, `postgame`, `perform`) over state
that holds the table. It is the only node that acts — `perform` calls `delegate` —
so the graph decides *who* and the rule decides *what to ask*.

```text
input → route → night|dawn|day|vote|exile|hunter|postgame → perform → route
                    ↑                                            │
                    └──────────── end when the table waits for you ┘
```

**Why seats need a hook.** Delegation gives a seat a fresh child session per call.
Without a fact of its own a seat would meet the table anew every night, so
`seat-memory` reads and writes the seat's file under `.forge/memory`, keyed by the
game named on the brief's first line. Only the engine knows when a turn starts and
ends; only the deployment knows what a seat may remember. That is the split the hook
kind exists for.

## Two shapes of director

A raid is one director and a cast. The two raids in this directory differ in one
axis: whether the director speaks.

| | werewolf (refereed) | storyteller (narrated) |
|---|---|---|
| director | `referee`: rules only, **no model call of its own** | `narrator`: two model nodes, `schedule` and `narrate` |
| cast | eight players, one of them a person | four characters, nobody |
| truth | the game state (`game`) | the blueprint and the progress (`story`) |
| ends when | the table waits for a person, or a side wins | the arc hands back to the reader, then closes |

The narrated raid is data as much as code: `rules/seed.star` deals a blueprint of ten
phases, each with a goal, a boundary, the speakers it allows and how many beats it
runs before handing back. A rule picks the candidate list, the scheduler model picks
one within it, and the rules reject anything outside it — the model phrases, it does
not decide the flow.

Two engine facts make that work:

- **`graph.node`** — the engine brackets every node invocation (start and end) on the
  event stream, so a surface knows whose words it is watching. A one-node step is
  attributable exactly; a wider frontier is a timeline, not an attribution.
- **`request.stream: false`** — a model call whose answer is data (a control line, a
  verdict) can ask for one finished response instead of deltas.

A deployment says which nodes do not speak for the reader, and a surface filters them:

```yaml
# story.yaml
internal: [schedule]        # the control line stays off the table
until: "故事收束"            # what an autoplay stops on
```

```bash
go run . test -test storyteller/opening --timeout 300s
go run . test-auto --raid storyteller --rounds 12
```

## Rules are scripts

A rule is a Starlark source, run in-process by `core/agent-graph/script`:

```yaml
- id: perform
  kind: script
  config:
    backend: starlark
    path: rules/perform.star
    reads: [game, brief]
    writes: [game, brief, spoken]
    allow: [tool:delegate]        # the one rule that acts
```

- **Declared, not ambient**: `reads`/`writes` are the rule's whole surface, and
  `allow` is every tool it may reach. A rule cannot touch a key or a tool the
  document did not name.
- **No suspend**: a script runs to completion. `allow` non-empty forbids suspending
  inside it, so a rule that acts is a rule that finishes.
- **`version: pinned`** keeps a resumed task on the version it recorded; if that
  version is gone the run fails loudly instead of quietly changing rules mid-game.
- Termination is by construction: no IO, no clock, budgets on steps and output
  size. Memory is an explicit non-goal — the language is chosen for determinism,
  not for a hostile sandbox.

The backend is language-neutral: `backend: starlark` ships first because it is
deterministic and has no IO; a Lua worker pool is the intended sibling, not a
replacement.

## Hooks: what the host owns

Three moments, declared per agent in `deploy.yaml`:

| Hook | When | Failure |
|---|---|---|
| `OnSession` | after a session opens or is restored, before its turn | stops the turn |
| `BeforeTurn` | before the engine sees the turn; may rewrite the message | stops the turn |
| `AfterTurn` | after the run ends, with state and result | **logged, not fatal** |

`AfterTurn` is the one that must not fail: the work is done and side effects cannot
be undone, so a memory write that loses a race is a log line, not a failed game.

## `carry`: state across the turns of a session

A graph's state can be declared `carry: true`, which starts the next task from the
previous one instead of from a zero value. The referee uses it for the table, and
`day.star` uses the speech channel to know a human answered.

It is for **bounded state** — a game, a form, a running total — not for transcripts.
Measured on an M2 Max (`core/agent-graph/carry_bench_test.go`): starting a task with
a fresh state costs ~14.7µs; carrying a 200-message transcript costs ~667µs and
~5k allocations per turn. A conversation belongs in one task, not in carried state.

## Reading a run

`forge inspect` folds the checkpoint store into a timeline, which is the difference
between "the seats abstained" and "the seats said 我投六号, and the parser read a
byte":

```text
step 4  perform  (live)
  script.run       rules/perform.star  v=sha256:aa471…  live
  script.tool      delegate#0 ok  我想先救1号，毒药留一晚。  args=sha256:e1dba49…
  script.tool      delegate#1 ok  我这票先弃了。  args=sha256:89c7b63…
  state game     brief_kind= brief_seat=0 current_votes=3 items vote_index=3
```

- one marker per step: the nodes that ran, and whether the step **replayed**;
- the state fields the step **changed**, not the whole document;
- each rule with its version, each tool call with its answer and argument hash.

`delegate#0` and `delegate#1` are the original answer and the re-ask: the rules
re-ask a seat whose vote they could not read, once, before recording an abstention.

## Where the code is

| Path | Job |
|---|---|
| `internal/app` | opens a workspace: manager, store, sessions, one turn |
| `internal/app/document.go` | reads `deploy.yaml`, `${workspace:root}`, missing env |
| `internal/app/drivers.go` | the driver table this binary compiles in |
| `internal/hooks/memory` | the `seat-memory` hook kind |
| `internal/story` | who the table is: director, speakers, labels, player seat |
| `internal/simtools` | the scenario's own tools, as a `tool.Source` |
| `internal/chatfmt` | events → speech/tool blocks, shared by TUI and test logs |
| `internal/tui` | the bubbletea interface |
| `internal/commands` | the CLI surface, including `inspect` |
| `internal/scenario` | file resolution and copying; nothing about agents |

The whole host is `host.New` + `Loader.Load` + `Deploy`:

```go
mgr := host.New(graphKinds(abs), host.Bindings{
    Store:       store,                                       // resume
    LoadOptions: workspaceOptions(abs),                       // ${workspace:root}
    OnEvent:     a.forwardSeat,                               // seats' streams
})
specs, err := mgr.Loader().Load(ctx, body)
_, err = mgr.Deploy(ctx, specs...)

session, _ := mgr.Session("referee", "referee/"+app.SessionID)   // a session belongs to one agent
for event, err := range session.Run(ctx, message.NewTextMessage(message.RoleUser, text)) { ... }
```

## Workspace layout

```text
scenarios/raids/werewolf/
  deploy.yaml              # nine agents: referee + eight seats
  runtime.yaml             # substrate: sandbox, files, models, tools
  story.yaml               # director, speakers, labels, player seat
  graphs/referee.yaml      # the game: state, eleven nodes, edges
  graphs/seat_1..8.yaml    # one persona graph per seat
  rules/*.star             # eleven rules
scenarios/tests/werewolf/opening.yaml, full_game.yaml
```

`${workspace:root}` is a placeholder scheme forge binds to the directory it opens, so
a scenario stays relocatable. A workspace keeps its durable side in
`.forge/sessions.db`; `tui resume` therefore resumes the conversation, not just the
directory.

## Differences from the previous forge

The previous version was written against `core` v0.10.0 and no longer compiles. This
one follows the current layering:

- **No resource document.** A deployment is agents plus engine settings plus runtime
  layers; there is no assembly step in the document.
- **Scripts are back, but not JavaScript.** The werewolf rules are Starlark, run
  in-process by a script node the graph engine owns. The old JS runtime stays
  retired; the old scenarios are kept in `../forge-tmp` for reference only.
- **No board.** Graph state replaces board variables, and conditions read
  `state.<key>`; the model is chosen in `runtime.yaml`, not by a TUI command.
- **Attribution is by agent.** Events say what was said and which tool ran, not
  which node produced it, so a deployment that needs per-speaker attribution gives
  each speaker its own agent — which is exactly what the seats are.
- **Usage and tool counts come from the event stream.**

## Development notes

`core` is not tagged at the revision this example needs, so the module resolves
through the repository's `go.work` and carries no `replace` of its own.

Gates, from this directory: `gofmt -l .`, `go build ./... && go vet ./... && go test ./...`.
From the repository root: `make ci` (and `make lint` for the CI-gated modules).
