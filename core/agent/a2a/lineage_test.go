package a2a

import (
	"context"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// TestEngineStampsRunLineageOnEvents verifies the A2A engine's own mint
// points (run start/end, step lifecycle) carry the run's delegation
// lineage headers, so envelopes published by an A2A-executed subagent
// preserve the run tree like the graph engine's do.
func TestEngineStampsRunLineageOnEvents(t *testing.T) {
	var mu sync.Mutex
	var envelopes []event.Envelope
	host := agent.HostFuncs{
		Inner: agent.NoopHost{},
		PublishFn: func(_ context.Context, env event.Envelope) error {
			mu.Lock()
			envelopes = append(envelopes, env)
			mu.Unlock()
			return nil
		},
	}
	eng, err := New(context.Background(), testCard(), WithStreamMode(StreamModeOff))
	if err != nil {
		t.Fatal(err)
	}
	run := agent.Run{
		Identity: agent.Identity{
			AgentID:     "a2a-agent",
			RunID:       "run-a2a",
			ParentRunID: "caller-run",
		},
		Attributes: map[string]string{telemetry.AttrToolCallID: "call-delegate-1"},
	}
	// An empty board completes as a local no-op without network I/O but
	// still exercises the run/step event mint points.
	if _, err := eng.Execute(context.Background(), run, host, agent.NewBoard()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(envelopes) == 0 {
		t.Fatal("engine published no envelopes")
	}
	for _, env := range envelopes {
		if env.ParentRunID() != "caller-run" || env.ToolCallID() != "call-delegate-1" {
			t.Fatalf("envelope %q lineage headers = %+v", env.Subject, env.Headers)
		}
	}
}

// TestEngineKeepsTopLevelRunHeaderFreeOnEvents guards the inverse: run
// events for a top-level run must stay free of parent/tool-call headers.
func TestEngineKeepsTopLevelRunHeaderFreeOnEvents(t *testing.T) {
	var mu sync.Mutex
	var envelopes []event.Envelope
	host := agent.HostFuncs{
		Inner: agent.NoopHost{},
		PublishFn: func(_ context.Context, env event.Envelope) error {
			mu.Lock()
			envelopes = append(envelopes, env)
			mu.Unlock()
			return nil
		},
	}
	eng, err := New(context.Background(), testCard(), WithStreamMode(StreamModeOff))
	if err != nil {
		t.Fatal(err)
	}
	run := agent.Run{Identity: agent.Identity{AgentID: "a2a-agent", RunID: "run-a2a"}}
	if _, err := eng.Execute(context.Background(), run, host, agent.NewBoard()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(envelopes) == 0 {
		t.Fatal("engine published no envelopes")
	}
	for _, env := range envelopes {
		if env.ParentRunID() != "" || env.ToolCallID() != "" {
			t.Fatalf("envelope %q lineage headers = %+v, want none", env.Subject, env.Headers)
		}
	}
}
