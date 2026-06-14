# Claw

Claw is a local workspace runner for graph-based conversational agents. The CLI
can create workspaces from embedded configs, open a TUI, serve debug APIs, and
run scripted tests. The Go SDK can embed the same runtime in another program.

## CLI Usage

Run commands from `cmd/claw` during development:

```sh
go run . help
```

Build a binary if you want a reusable local command:

```sh
go build -o claw .
./claw help
```

### Configs

Claw syncs embedded configs into the user config directory on startup. List the
available config names with:

```sh
claw config raid list
claw config persona list
claw config test list
```

Configs are grouped as raid, persona, and test configs. A config name such as
`journey` or `murder_mystery` can be used wherever a command accepts
`<name|path>`. A filesystem path to a YAML config is also accepted.

### Workspaces

Create a workspace from a config:

```sh
claw workspace create --config journey --workspace ./workspace
```

Inspect an existing workspace:

```sh
claw workspace inspect --workspace ./workspace
```

A workspace contains `config.yaml` plus runtime state such as history, memory,
and persisted graph state. The config controls the agent graph, model aliases,
history, and memory behavior.

### TUI

Start a new TUI session by selecting a raid config:

```sh
claw tui new
```

Resume an existing local workspace:

```sh
claw tui resume
```

The TUI has a recall panel, chat panel, and workspace/debug panel. Press `tab`
to switch focus, `enter` to submit, and `q` or `esc` to quit.

### Debug API

Serve the debug HTTP API for a workspace:

```sh
claw serve --workspace ./workspace --addr 127.0.0.1:8787
```

Available endpoints:

```text
GET  /debug
GET  /debug/workspace
GET  /debug/history?context_id=__default__
GET  /debug/memory
POST /debug/recall
```

Example recall request:

```sh
curl -s http://127.0.0.1:8787/debug/recall \
  -H 'content-type: application/json' \
  -d '{"text":"what happened in the story?","top_k":5}'
```

### Tests

Run one embedded scripted test:

```sh
claw test -test journey/opening_continue --timeout 2m
```

Run a raid/persona simulation:

```sh
claw test-auto --raid journey --persona boy_14_Tom --timeout 5m
```

Test output is written under `.out/` with a workspace snapshot, chat log, and
stats files.

## SDK Usage

Import the local runtime from `sdkx/claw` and back it with an SDK workspace.

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/claw"
)

func main() {
	ws, err := workspace.NewLocalWorkspace("./workspace")
	if err != nil {
		panic(err)
	}

	app, err := claw.New(ws)
	if err != nil {
		panic(err)
	}
	defer app.Close()

	resp, err := app.RoundTrip(claw.Request{
		Context: context.Background(),
		Text:    "continue the story",
	})
	if err != nil {
		panic(err)
	}

	for {
		ev, err := resp.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			panic(err)
		}
		switch ev.Type {
		case claw.EventToken:
			fmt.Print(ev.Content)
		case claw.EventStatus:
			fmt.Printf("\n[%s]\n", ev.Content)
		case claw.EventError:
			panic(ev.Err)
		}
	}
}
```

### Workspace Contract

`claw.New` expects the workspace to contain `config.yaml`. CLI-created
workspaces already have this file. In an embedded app, you can create or copy the
config before constructing the runtime.

### Tools

Tool schemas are declared in config. Runtime implementations are registered in
Go. Imports are omitted in this short snippet:

```go
app.Handle("play_music", func(ctx context.Context, name string, args json.RawMessage) (string, error) {
	return `{"ok":true}`, nil
})

app.HandleDefault(func(ctx context.Context, name string, args json.RawMessage) (string, error) {
	return `{"ok":true}`, nil
})
```

### Debug Handler

Embed the same debug API in another server:

```go
handler := claw.NewDebugHTTPHandler(app)
http.ListenAndServe("127.0.0.1:8787", handler)
```

### History And Speaker Metadata

When history is enabled, visible assistant output is persisted under the
workspace history root. Claw stores visible node metadata as an XML message part:

```json
{"type":"text/xml","text":"<node id=\"storyteller_origin\" />"}
```

An assistant message can also start with a speaker directive:

```xml
<speaker name="沈知秋" />I was in the study that night.
```

Claw persists the speaker as a `text/xml` part and keeps the remaining content as
normal text. On later turns, the XML part is sent back as normal text context so
the model can see the prior speaker boundary.
