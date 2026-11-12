package script_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/agent-graph/script/starlark"
	"github.com/GizClaw/flowcraft/core/message"
)

func benchKind(tb testing.TB, source string) graph.NodeKind {
	tb.Helper()
	kind, err := script.New(
		script.WithBackends(starlark.New()),
		script.WithSource(staticSource{"rule.star": source}),
		script.WithLog(io.Discard),
	)
	if err != nil {
		tb.Fatalf("script.New: %v", err)
	}
	return kind
}

// benchState is one declared state with a game object and a seat list, which is
// the shape a turn-based rule reads.
func benchState(tb testing.TB, seats int) map[string]graph.Value {
	tb.Helper()
	seatsJSON, err := json.Marshal([]string{"a", "b", "c", "d"}[:seats])
	if err != nil {
		tb.Fatalf("marshal seats: %v", err)
	}
	return map[string]graph.Value{
		"game":   graph.JSON([]byte(`{"phase":"day","day":2}`)),
		"seats":  graph.JSON(seatsJSON),
		"answer": graph.Text(""),
	}
}

func benchInvoke(b *testing.B, kind graph.NodeKind, in graph.Invoke) {
	b.Helper()
	stream, err := kind.Invoke(context.Background(), in)
	if err != nil {
		b.Fatalf("invoke: %v", err)
	}
	for {
		if _, err := stream.Next(context.Background()); err != nil {
			if !errors.Is(err, io.EOF) {
				b.Fatalf("next: %v", err)
			}
			return
		}
	}
}

// benchInput builds the Invoke the engine passes: the config is decoded once at
// Build, so the invocation carries the decoded plan.
func benchInput(tb testing.TB, kind graph.NodeKind, values map[string]graph.Value, config string) graph.Invoke {
	tb.Helper()
	decoder, ok := kind.(graph.ConfigDecoder)
	if !ok {
		tb.Fatal("the kind must decode its own config")
	}
	decoded, err := decoder.DecodeConfig(json.RawMessage(config))
	if err != nil {
		tb.Fatalf("DecodeConfig: %v", err)
	}
	return graph.Invoke{
		Node:     "tally",
		Decoded:  decoded,
		Config:   json.RawMessage(config),
		State:    func(key string) graph.Value { return values[key] },
		Snapshot: func() graph.Snapshot { return graph.NewSnapshot(values) },
	}
}

// BenchmarkNodeInvoke is the unit a step pays for a rule: projection, one
// compiled program, write validation and the records.
func BenchmarkNodeInvoke(b *testing.B) {
	kind := benchKind(b, `
def run(state):
    votes = {}
    for seat in state["seats"]:
        votes[seat] = votes.get(seat, 0) + 1
    state["game"]["votes"] = votes
    return {"game": state["game"]}
`)
	in := benchInput(b, kind, benchState(b, 4),
		`{"backend":"starlark","path":"rule.star","reads":["game","seats"],"writes":["game"]}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchInvoke(b, kind, in)
	}
}

// BenchmarkNodeInvokeTranscript measures the projection a chat-shaped rule pays:
// the transcript is re-read and re-encoded on every step, which is where a growing
// session shows up.
func BenchmarkNodeInvokeTranscript(b *testing.B) {
	kind := benchKind(b, `
def run(state):
    return {"answer": str(len(state["history"]))}
`)
	history := make([]message.Message, 0, 200)
	for i := 0; i < 200; i++ {
		role := message.RoleUser
		if i%2 == 1 {
			role = message.RoleAssistant
		}
		history = append(history, message.NewTextMessage(role, "这是对话里的一条消息，用来撑出真实的投影成本。"))
	}
	values := map[string]graph.Value{
		"history": graph.MessagesValue(graph.MessagesFromSlice(history)),
		"answer":  graph.Text(""),
		"game":    graph.JSON([]byte(`{}`)),
		"seats":   graph.JSON([]byte(`[]`)),
	}
	in := benchInput(b, kind, values,
		`{"backend":"starlark","path":"rule.star","reads":["history"],"writes":["answer"]}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchInvoke(b, kind, in)
	}
}

// BenchmarkNodeInvokeReplay measures the cheap path a replay takes: the tool call
// is answered from its record, so the runtime and the interpreter's globals are
// the only work left.
func BenchmarkNodeInvokeReplay(b *testing.B) {
	kind, err := script.New(
		script.WithBackends(starlark.New()),
		script.WithRuntime(toolRuntime(b)),
		script.WithSource(staticSource{"rule.star": `
def run(state):
    return {"answer": tool("echo", {})["text"]}
`}),
		script.WithLog(io.Discard),
	)
	if err != nil {
		b.Fatalf("script.New: %v", err)
	}
	payload, err := json.Marshal(script.ToolRecord{
		Name:          "echo",
		ArgumentsHash: argumentsHash(b, map[string]any{}),
		Index:         0,
		OK:            true,
		Text:          "from the record",
	})
	if err != nil {
		b.Fatalf("marshal record: %v", err)
	}
	in := benchInput(b, kind, benchState(b, 4),
		`{"backend":"starlark","path":"rule.star","reads":[],"writes":["answer"],"allow":["tool:echo"]}`)
	in.Node = "cue"
	in.Replay = true
	in.Records = func(name string) []agent.Record {
		if name == script.RecordTool {
			return []agent.Record{{Kind: script.RecordTool, Payload: payload}}
		}
		return nil
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchInvoke(b, kind, in)
	}
}

// BenchmarkGraphScriptNode puts a rule behind the frontier loop, so the engine's
// per-step cost is visible next to the node's.
func BenchmarkGraphScriptNode(b *testing.B) {
	kind := benchKind(b, `
def run(state):
    return {"answer": "dawn"}
`)
	registry, err := graph.NewRegistry(kind)
	if err != nil {
		b.Fatalf("NewRegistry: %v", err)
	}
	document, err := graph.Parse([]byte(`
version: 1
entry: rule
state:
  game: {type: json}
  answer: {type: text}
nodes:
  - id: rule
    kind: script
    config:
      backend: starlark
      path: rule.star
      reads: []
      writes: [answer]
edges:
  - {from: rule, to: end}
output: {key: answer}
`))
	if err != nil {
		b.Fatalf("Parse: %v", err)
	}
	built, err := graph.Build(document, registry)
	if err != nil {
		b.Fatalf("Build: %v", err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		run, err := built.Start(context.Background(), nil)
		if err != nil {
			b.Fatalf("Start: %v", err)
		}
		for _, err := range run.Events(context.Background()) {
			if err != nil {
				b.Fatalf("run: %v", err)
			}
		}
	}
}
