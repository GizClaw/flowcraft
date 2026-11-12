package graph

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
)

// TestStateChangesReachTheStream pins what a surface needs to classify what it just
// rendered: one event per step, naming the keys the step wrote, in the domain a host
// reads rather than the checkpoint's internal encoding.
func TestStateChangesReachTheStream(t *testing.T) {
	g := build(t, `
version: 1
entry: write
state:
  text: {type: text}
nodes:
  - {id: write, kind: const, config: {key: text, value: hello}}
edges:
  - {from: write, to: end}
`)
	run, err := g.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var states []GraphState
	for event, err := range run.Events(context.Background()) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		extension, ok := event.(agent.EventExtension[GraphState])
		if !ok {
			continue
		}
		states = append(states, extension.Value)
	}
	if len(states) != 1 {
		t.Fatalf("state events = %d, want one for the one step", len(states))
	}
	if states[0].Step != 1 {
		t.Fatalf("step = %d", states[0].Step)
	}
	raw, ok := states[0].Values["text"]
	if !ok {
		t.Fatalf("values = %v, want the key the step wrote", states[0].Values)
	}
	// The value is the text itself, not the checkpoint's {kind, text} envelope.
	var text string
	if err := json.Unmarshal(raw, &text); err != nil || text != "hello" {
		t.Fatalf("text = %s (%v)", raw, err)
	}
}

// TestStateEventsStayOffTheCheckpoint is the other half: the stream is for
// observers, and the store must not grow a second copy of what it already holds.
func TestStateEventsStayOffTheCheckpoint(t *testing.T) {
	document := `
version: 1
entry: write
state:
  text: {type: text}
nodes:
  - {id: write, kind: const, config: {key: text, value: hello}}
edges:
  - {from: write, to: end}
`
	g := build(t, document)
	store := agenttest.NewMemStore()
	run, err := g.Start(context.Background(), nil, WithStore(store, "task-1"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := drainRun(t, run); err != nil {
		t.Fatalf("run: %v", err)
	}
	records, err := store.Events(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, record := range records {
		if record.Kind == string(agent.KindExtension) {
			t.Fatalf("the checkpoint holds an extension event: %s", record.Kind)
		}
	}
}
