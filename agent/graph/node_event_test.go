package graph

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
)

// TestEachInvocationIsBracketed pins the engine's own attribution: a node's start
// precedes its events and its end follows them, so a surface can tell which node it
// is watching while a step runs.
func TestEachInvocationIsBracketed(t *testing.T) {
	g := build(t, `
version: 1
entry: first
state:
  count: {type: json}
nodes:
  - {id: first, kind: bump, config: {key: count}}
  - {id: second, kind: bump, config: {key: count}}
edges:
  - {from: first, to: second}
  - {from: second, to: end}
`, bumpKind{})
	run, err := g.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	events, err := drainRun(t, run)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var marks []NodeEvent
	for _, event := range events {
		if extension, ok := event.(agent.EventExtension[NodeEvent]); ok {
			marks = append(marks, extension.Value)
		}
	}
	want := []struct {
		step  int
		node  string
		phase NodePhase
	}{
		{1, "first", NodeStarted},
		{1, "first", NodeDone},
		{2, "second", NodeStarted},
		{2, "second", NodeDone},
	}
	if len(marks) != len(want) {
		t.Fatalf("node events = %d, want %d: %+v", len(marks), len(want), marks)
	}
	for i, expected := range want {
		got := marks[i]
		if got.Step != expected.step || got.Node != expected.node || got.Phase != expected.phase {
			t.Fatalf("event %d = %+v, want %+v", i, got, expected)
		}
		if got.Kind != "bump" {
			t.Fatalf("event %d kind = %q", i, got.Kind)
		}
	}
}
