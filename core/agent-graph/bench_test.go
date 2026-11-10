package graph

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// benchKind writes one boolean, so a benchmark measures the engine rather than a
// node.
type benchKind struct{}

func (benchKind) Name() string { return "bench" }

func (benchKind) Invoke(context.Context, Invoke) (NodeStream, error) {
	return Static(Result{State: []StateUpdate{{Key: "flag", Value: Bool(true)}}}), nil
}

const benchDoc = `
version: 1
entry: a
state:
  flag: {type: bool}
nodes:
  - {id: a, kind: bench}
edges:
  - {from: a, to: end}
`

func benchmarkGraph(b *testing.B) *Graph {
	b.Helper()
	document, err := Parse([]byte(benchDoc))
	if err != nil {
		b.Fatalf("Parse: %v", err)
	}
	registry, err := NewRegistry(benchKind{})
	if err != nil {
		b.Fatalf("NewRegistry: %v", err)
	}
	graph, err := Build(document, registry)
	if err != nil {
		b.Fatalf("Build: %v", err)
	}
	return graph
}

// BenchmarkRunOneStep is the whole loop for one frontier: start, invoke, merge,
// route and report the status.
func BenchmarkRunOneStep(b *testing.B) {
	g := benchmarkGraph(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		run, err := g.Start(context.Background(), nil)
		if err != nil {
			b.Fatalf("Start: %v", err)
		}
		for _, err := range run.Events(context.Background()) {
			if err != nil {
				b.Fatalf("Events: %v", err)
			}
		}
	}
}

// BenchmarkCondition is one edge evaluation against a ten-key snapshot. The
// environment is rebuilt per step, so this is the routing cost the loop pays.
func BenchmarkCondition(b *testing.B) {
	condition, err := CompileCondition(`state.k0 == "v" and step < 100`)
	if err != nil {
		b.Fatalf("CompileCondition: %v", err)
	}
	values := make(map[string]Value, 10)
	for i := 0; i < 10; i++ {
		values["k"+string(rune('0'+i))] = Text("v")
	}
	snapshot := NewSnapshot(values)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := condition.Evaluate(1, snapshot); err != nil {
			b.Fatalf("Evaluate: %v", err)
		}
	}
}

// BenchmarkMessagesFromSlice rebuilds a transcript the way a restore does: one
// node per message, sharing the prefix.
func BenchmarkMessagesFromSlice(b *testing.B) {
	messages := make([]message.Message, 10_000)
	for i := range messages {
		messages[i] = message.NewTextMessage(message.RoleUser, "turn")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if got := MessagesFromSlice(messages).Len(); got != len(messages) {
			b.Fatalf("Len = %d", got)
		}
	}
}
