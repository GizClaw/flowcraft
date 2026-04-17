package stream

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/kanban"
)

func TestMapEvent(t *testing.T) {
	ts := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	base := event.Event{
		RunID:     "run-1",
		GraphID:   "graph-1",
		NodeID:    "node-1",
		Timestamp: ts,
	}

	tests := []struct {
		name         string
		ev           event.Event
		wantOK       bool
		wantType     string
		checkPayload func(t *testing.T, p map[string]any)
	}{
		{
			name: "graph_start",
			ev: func() event.Event {
				e := base
				e.Type = event.EventGraphStart
				e.Payload = map[string]any{"key": "val"}
				return e
			}(),
			wantOK:   true,
			wantType: "graph_start",
			checkPayload: func(t *testing.T, p map[string]any) {
				vars, ok := p["vars"].(map[string]any)
				if !ok {
					t.Fatal("vars missing or wrong type")
				}
				if vars["key"] != "val" {
					t.Errorf("vars[key] = %v, want val", vars["key"])
				}
			},
		},
		{
			name: "node_start",
			ev: func() event.Event {
				e := base
				e.Type = event.EventNodeStart
				e.Payload = 3
				return e
			}(),
			wantOK:   true,
			wantType: "node_start",
			checkPayload: func(t *testing.T, p map[string]any) {
				if p["node_id"] != "node-1" {
					t.Errorf("node_id = %v, want node-1", p["node_id"])
				}
			},
		},
		{
			name: "node_complete",
			ev: func() event.Event {
				e := base
				e.Type = event.EventNodeComplete
				e.Payload = map[string]any{"output": "done"}
				return e
			}(),
			wantOK:   true,
			wantType: "node_complete",
			checkPayload: func(t *testing.T, p map[string]any) {
				if p["node_id"] != "node-1" {
					t.Errorf("node_id = %v, want node-1", p["node_id"])
				}
				if p["output"] != "done" {
					t.Errorf("output = %v, want done", p["output"])
				}
			},
		},
		{
			name: "node_skipped",
			ev: func() event.Event {
				e := base
				e.Type = event.EventNodeSkipped
				e.Payload = "condition false"
				return e
			}(),
			wantOK:   true,
			wantType: "node_skipped",
			checkPayload: func(t *testing.T, p map[string]any) {
				if p["node_id"] != "node-1" {
					t.Errorf("node_id = %v, want node-1", p["node_id"])
				}
				if p["reason"] != "condition false" {
					t.Errorf("reason = %v, want 'condition false'", p["reason"])
				}
			},
		},
		{
			name: "node_error",
			ev: func() event.Event {
				e := base
				e.Type = event.EventNodeError
				e.Payload = map[string]any{"error": "boom"}
				return e
			}(),
			wantOK:   true,
			wantType: "node_error",
			checkPayload: func(t *testing.T, p map[string]any) {
				if p["node_id"] != "node-1" {
					t.Errorf("node_id = %v, want node-1", p["node_id"])
				}
				if p["error"] != "boom" {
					t.Errorf("error = %v, want boom", p["error"])
				}
			},
		},
		{
			name: "parallel_fork",
			ev: func() event.Event {
				e := base
				e.Type = event.EventParallelFork
				e.Payload = map[string]any{"branches": 3}
				return e
			}(),
			wantOK:   true,
			wantType: "parallel_fork",
		},
		{
			name: "parallel_join",
			ev: func() event.Event {
				e := base
				e.Type = event.EventParallelJoin
				e.Payload = map[string]any{"branches": 3}
				return e
			}(),
			wantOK:   true,
			wantType: "parallel_join",
		},
		{
			name: "checkpoint",
			ev: func() event.Event {
				e := base
				e.Type = event.EventCheckpoint
				e.Payload = map[string]any{"snapshot": true}
				return e
			}(),
			wantOK:   true,
			wantType: "checkpoint",
			checkPayload: func(t *testing.T, p map[string]any) {
				if p["node_id"] != "node-1" {
					t.Errorf("node_id = %v, want node-1", p["node_id"])
				}
				if p["state"] == nil {
					t.Error("state should not be nil")
				}
			},
		},
		{
			name: "kanban_update",
			ev: func() event.Event {
				e := base
				e.Type = event.EventKanbanUpdate
				e.Payload = map[string]any{"cards": 5}
				return e
			}(),
			wantOK:   true,
			wantType: "kanban_update",
		},
		{
			name: "kanban_task_submitted",
			ev: func() event.Event {
				e := base
				e.Type = "kanban.task.submitted"
				e.Payload = map[string]any{"card_id": "c1"}
				return e
			}(),
			wantOK:   true,
			wantType: "kanban_update",
			checkPayload: func(t *testing.T, p map[string]any) {
				if p["event_type"] != "kanban.task.submitted" {
					t.Errorf("event_type = %v, want kanban.task.submitted", p["event_type"])
				}
			},
		},
		{
			name: "approval_required",
			ev: func() event.Event {
				e := base
				e.Type = event.EventApprovalRequired
				e.Payload = map[string]any{"tool": "rm -rf"}
				return e
			}(),
			wantOK:   true,
			wantType: "approval_required",
			checkPayload: func(t *testing.T, p map[string]any) {
				if p["node_id"] != "node-1" {
					t.Errorf("node_id = %v, want node-1", p["node_id"])
				}
			},
		},
		{
			name: "graph_end",
			ev: func() event.Event {
				e := base
				e.Type = event.EventGraphEnd
				e.Payload = map[string]any{"status": "ok"}
				return e
			}(),
			wantOK:   true,
			wantType: "graph_end",
		},
		{
			name: "graph_changed",
			ev: func() event.Event {
				e := base
				e.Type = event.EventGraphChanged
				e.Payload = map[string]any{"diff": true}
				return e
			}(),
			wantOK:   true,
			wantType: "graph_changed",
		},
		{
			name: "agent_config_changed",
			ev: func() event.Event {
				e := base
				e.Type = event.EventAgentConfigChanged
				e.Payload = map[string]any{"model": "gpt-5"}
				return e
			}(),
			wantOK:   true,
			wantType: "agent_config_changed",
		},
		{
			name: "compile_result",
			ev: func() event.Event {
				e := base
				e.Type = event.EventCompileResult
				e.Payload = map[string]any{"errors": 0}
				return e
			}(),
			wantOK:   true,
			wantType: "compile_result",
		},
		{
			name: "unknown_event_discarded",
			ev: func() event.Event {
				e := base
				e.Type = "some.unknown.type"
				e.Payload = "irrelevant"
				return e
			}(),
			wantOK: false,
		},
		{
			name: "nil_payload",
			ev: func() event.Event {
				e := base
				e.Type = event.EventGraphStart
				e.Payload = nil
				return e
			}(),
			wantOK:   true,
			wantType: "graph_start",
			checkPayload: func(t *testing.T, p map[string]any) {
				if p["vars"] != nil {
					t.Errorf("vars should be nil, got %v", p["vars"])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := MapEvent(tt.ev)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", got.Type, tt.wantType)
			}
			if got.Payload["run_id"] != tt.ev.RunID {
				t.Errorf("run_id = %v, want %v", got.Payload["run_id"], tt.ev.RunID)
			}
			if got.Payload["graph_id"] != tt.ev.GraphID {
				t.Errorf("graph_id = %v, want %v", got.Payload["graph_id"], tt.ev.GraphID)
			}
			if _, ok := got.Payload["timestamp"]; !ok {
				t.Error("timestamp missing")
			}
			if tt.checkPayload != nil {
				tt.checkPayload(t, got.Payload)
			}
		})
	}
}

func TestMapEvent_StreamDelta(t *testing.T) {
	ts := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	base := event.Event{
		Type:      event.EventStreamDelta,
		RunID:     "run-1",
		GraphID:   "graph-1",
		NodeID:    "llm-1",
		Timestamp: ts,
	}

	tests := []struct {
		name         string
		delta        map[string]any
		wantOK       bool
		wantType     string
		checkPayload func(t *testing.T, p map[string]any)
	}{
		{
			name:     "token",
			delta:    map[string]any{"type": "token", "content": "hello"},
			wantOK:   true,
			wantType: "agent_token",
			checkPayload: func(t *testing.T, p map[string]any) {
				if p["node_id"] != "llm-1" {
					t.Errorf("node_id = %v, want llm-1", p["node_id"])
				}
				if p["chunk"] != "hello" {
					t.Errorf("chunk = %v, want hello", p["chunk"])
				}
			},
		},
		{
			name: "tool_call",
			delta: map[string]any{
				"type": "tool_call", "id": "tc-1",
				"name": "search", "arguments": `{"q":"test"}`,
			},
			wantOK:   true,
			wantType: "agent_tool_call",
			checkPayload: func(t *testing.T, p map[string]any) {
				if p["tool_call_id"] != "tc-1" {
					t.Errorf("tool_call_id = %v, want tc-1", p["tool_call_id"])
				}
				if p["tool_name"] != "search" {
					t.Errorf("tool_name = %v, want search", p["tool_name"])
				}
			},
		},
		{
			name: "tool_result",
			delta: map[string]any{
				"type": "tool_result", "tool_call_id": "tc-1",
				"name": "search", "content": "found it", "is_error": false,
			},
			wantOK:   true,
			wantType: "agent_tool_result",
			checkPayload: func(t *testing.T, p map[string]any) {
				if p["tool_call_id"] != "tc-1" {
					t.Errorf("tool_call_id = %v, want tc-1", p["tool_call_id"])
				}
				if p["tool_result"] != "found it" {
					t.Errorf("tool_result = %v, want 'found it'", p["tool_result"])
				}
			},
		},
		{name: "unknown_delta_type", delta: map[string]any{"type": "unknown"}, wantOK: false},
		{name: "empty_delta", delta: map[string]any{}, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := base
			ev.Payload = tt.delta
			got, ok := MapEvent(ev)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", got.Type, tt.wantType)
			}
			if tt.checkPayload != nil {
				tt.checkPayload(t, got.Payload)
			}
		})
	}
}

func TestMapEvent_StreamDelta_NonMapPayload(t *testing.T) {
	ev := event.Event{Type: event.EventStreamDelta, RunID: "r", GraphID: "g", NodeID: "n", Timestamp: time.Now(), Payload: "not a map"}
	if _, ok := MapEvent(ev); ok {
		t.Fatal("expected ok=false")
	}
}

func TestMapEvent_StreamDelta_NilPayload(t *testing.T) {
	ev := event.Event{Type: event.EventStreamDelta, RunID: "r", GraphID: "g", NodeID: "n", Timestamp: time.Now()}
	if _, ok := MapEvent(ev); ok {
		t.Fatal("expected ok=false")
	}
}

func TestMapEvent_CallbackStart(t *testing.T) {
	ev := event.Event{
		Type: event.EventType(kanban.EventCallbackStart), RunID: "run-1", GraphID: "graph-1", Timestamp: time.Now(),
		Payload: kanban.CallbackStartPayload{CardID: "card-1", RuntimeID: "owner", AgentID: "agent-1", Query: "do something"},
	}
	got, ok := MapEvent(ev)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.Type != "callback_start" {
		t.Errorf("Type = %q, want callback_start", got.Type)
	}
	if got.Payload["card_id"] != "card-1" {
		t.Errorf("card_id = %v, want card-1", got.Payload["card_id"])
	}
	if got.Payload["runtime_id"] != "owner" {
		t.Errorf("runtime_id = %v, want owner", got.Payload["runtime_id"])
	}
}

func TestMapEvent_CallbackDone(t *testing.T) {
	ev := event.Event{
		Type: event.EventType(kanban.EventCallbackDone), RunID: "run-1", GraphID: "graph-1", Timestamp: time.Now(),
		Payload: kanban.CallbackDonePayload{CardID: "card-1", RuntimeID: "owner", AgentID: "agent-1", Error: "callback failed"},
	}
	got, ok := MapEvent(ev)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.Type != "callback_done" {
		t.Errorf("Type = %q, want callback_done", got.Type)
	}
	if got.Payload["runtime_id"] != "owner" {
		t.Errorf("runtime_id = %v, want owner", got.Payload["runtime_id"])
	}
	if got.Payload["error"] != "callback failed" {
		t.Errorf("error = %v, want callback failed", got.Payload["error"])
	}
}

func TestMapEvent_CallbackStart_WrongPayloadType(t *testing.T) {
	ev := event.Event{Type: event.EventType(kanban.EventCallbackStart), Timestamp: time.Now(), Payload: "wrong"}
	if _, ok := MapEvent(ev); ok {
		t.Fatal("expected ok=false")
	}
}

func TestMapEvent_CallbackDone_WrongPayloadType(t *testing.T) {
	ev := event.Event{Type: event.EventType(kanban.EventCallbackDone), Timestamp: time.Now(), Payload: 42}
	if _, ok := MapEvent(ev); ok {
		t.Fatal("expected ok=false")
	}
}
