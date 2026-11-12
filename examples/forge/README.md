# Forge — Runnable Local Workspace Demo

Forge is the runnable local demo for the FlowCraft stack. It turns a workspace
directory into deployed agents, drives one session from a terminal UI or a
scripted test, and keeps every scenario fact in plain files under `scenarios/`.

It is written against the current core layering on purpose: it is the smallest
complete host, so it shows what an application actually has to write.

中文版见 [README_zh.md](README_zh.md)。

## Quickstart

Prerequisites: Go 1.26+ and an OpenAI credential.

```bash
cd examples/forge
cp .env.example .env         # then set OPENAI_API_KEY=...
go run . help
go run . tui new             # pick "companion" and start chatting
go run . test -test companion/opening
```

## Commands

| Command | What it does |
|---|---|
| `forge workspace create --config <raid> --workspace <dir>` | copy a raid scenario into a workspace |
| `forge workspace inspect --workspace <dir>` | print the agents, engines and missing environment |
| `forge config raid\|persona\|test list` | list available scenarios and tests |
| `forge tui new` / `forge tui resume` | open the interactive TUI over a new or an existing workspace |
| `forge test -test <raid>/<name> [--timeout 2m]` | run one scripted test, writing `.out/<run>/` |
| `forge test-auto --raid <raid> --persona <persona> [--turns 3]` | simulate a dialogue between two deployed agents |

Scenario files are resolved in priority order from `--scenarios`, `FORGE_SCENARIOS`,
the executable's directory, the working directory, and the per-user config
directory.

## How a workspace is put together

```
scenarios/raids/companion/
  deploy.yaml            # which agents exist, and how each is built
  runtime.yaml           # the substrate: boundary, files, models, tools
  graphs/assistant.yaml  # one graph: state, nodes, edges
scenarios/personas/tom/  # the same three files, one agent, no tools
scenarios/tests/companion/opening.yaml
```

`deploy.yaml` names the agents. It does not describe resources: there is no
resource graph, no assembly step and no dependency injection in the document:

```yaml
version: 1
agents:
  - name: assistant
    engine:
      kind: graph
      settings:
        path: ${workspace:root}/graphs/assistant.yaml
    runtime:
      layers:
        - priority: 0
          path: ${workspace:root}/runtime.yaml
    catalog: {scope: session}
```

`${workspace:root}` is a placeholder scheme forge registers for the directory it
is opening (`internal/app/document.go`), so a scenario stays relocatable.

`runtime.yaml` is one `runtime.Config`: the sandbox backend, the writable roots,
the models (a driver ships no line-up), and which built-in tool sets are wired.
The simulated device tools are *not* here — they are a `tool.Source` the host
mounts (`internal/simtools`), which is how an application adds its own tools.

`graphs/assistant.yaml` is the agent itself: declared state keys with merge
rules, nodes, and edges whose conditions are expressions over that state:

```yaml
state:
  messages: {type: messages, merge: append}
  pending:  {type: json}
nodes:
  - {id: model, kind: model, config: {messages: messages, pending: pending, output: answer}}
  - {id: tools, kind: tool,  config: {messages: messages, pending: pending}}
edges:
  - {from: model, to: tools, when: len(state.pending) > 0}
  - {from: tools, to: model}
  - {from: model, to: end}
output: {key: answer}
```

## Where the code is

| Path | Job |
|---|---|
| `internal/app/app.go` | opens a workspace: manager, store, session, run iterator |
| `internal/app/document.go` | reads `deploy.yaml`, resolves `${workspace:root}`, reports missing env |
| `internal/app/drivers.go` | the driver table this binary compiles in (`openai`, `anthropic`) |
| `internal/app/usage.go` | folds a turn's usage events into one report |
| `internal/simtools` | the demo's own tools, as a `tool.Source` |
| `internal/chatfmt` | event stream → speech / tool blocks, shared by TUI and test logs |
| `internal/tui` | the bubbletea interface |
| `internal/commands` | the CLI surface |
| `internal/scenario` | file resolution and copying, nothing about agents |

The whole host is `host.New(...)` + `Loader.Load` + `Manager.Deploy`:

```go
mgr := host.New(deploy.BuiltinKinds(), host.Bindings{
    Store:          store,                                             // resume
    RuntimeOptions: []runtime.Option{drivers, simtools.New()},         // wiring
    LoadOptions:    workspaceOptions(dir),                             // ${workspace:root}
})
specs, err := mgr.Loader().Load(ctx, body)
_, err = mgr.Deploy(ctx, specs...)

session, _ := mgr.Session("assistant", app.SessionID)
for event, err := range session.Run(ctx, message.NewTextMessage(message.RoleUser, text)) { ... }
result, err := session.Result(ctx)
```

## Differences from the previous forge

The previous version was written against `core` v0.10.0 and no longer compiles
against the current layering. This rewrite follows it:

- **No resource document.** `resources:` / `deps:` / `kind: …impl: …` are gone;
  a deployment is agents plus engine settings plus runtime layers.
- **No JS script runtime.** The graph's built-in node kinds are `model`, `tool`
  and `condition`. The old `werewolf` and `multi_role_storyteller` scenarios were
  driven by ~2.3k lines of JavaScript and are kept in `../forge-tmp` for
  reference only; nothing in core executes scripts now.
- **No board.** Graph state replaces board variables, and conditions read
  `state.<key>`. A turn therefore cannot override the model: the model node's
  request is configuration, so `/model` and `/think` are gone from the TUI and
  the model is chosen in `runtime.yaml`.
- **No per-node speaker labels.** Events say what the model said and which tool
  ran, not which node produced it. A deployment that needs attribution gives each
  speaker its own agent — one session per speaker — and labels by agent name.
- **Tool counters and usage come from the event stream**, not from a decorator
  around a host factory.
- **Sessions are durable.** Each workspace keeps a SQLite store under
  `.forge/sessions.db`, so `tui resume` resumes the conversation, not just the
  directory.

## Development notes

The core revision this example needs is not tagged yet: `core` is at `v0.10.0`,
which predates `core/agent-host` and `core/agent-graph/nodes`. The module
therefore resolves through the repository's `go.work` — it carries no `replace`
directives of its own. Bump the `core` requirement when the next release is
tagged; `make tidy` and `make check-consumers` (which build with `GOWORK=off`
against released modules) only pass from that point on.

Gates, from this directory (workspace mode):

```bash
gofmt -l .
go build ./... && go vet ./... && go test ./...
```

and `make ci` from the repository root.
