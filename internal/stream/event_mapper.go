package stream

import (
	"encoding/json"
	"time"

	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/kanban"
)

// MappedEvent is the standardized output of MapEvent.
type MappedEvent struct {
	Type    string
	Payload map[string]any
}

// MapEvent converts an internal event.Event to the frontend-facing format.
// Returns ok=false when the event should be silently discarded.
func MapEvent(ev event.Event) (MappedEvent, bool) {
	var sseEvent string
	var data any

	switch ev.Type {
	case event.EventGraphStart:
		sseEvent = "graph_start"
		data = map[string]any{"vars": ev.Payload}
	case event.EventNodeStart:
		sseEvent = "node_start"
		data = map[string]any{"node_id": ev.NodeID, "iteration": ev.Payload}
	case event.EventNodeComplete:
		sseEvent = "node_complete"
		data = withNodeID(ev.NodeID, ev.Payload)
	case event.EventNodeSkipped:
		sseEvent = "node_skipped"
		data = map[string]any{"node_id": ev.NodeID, "reason": ev.Payload}
	case event.EventNodeError:
		sseEvent = "node_error"
		data = withNodeID(ev.NodeID, ev.Payload)
	case event.EventStreamDelta:
		return mapStreamDelta(ev)
	case event.EventParallelFork:
		sseEvent = "parallel_fork"
		data = ev.Payload
	case event.EventParallelJoin:
		sseEvent = "parallel_join"
		data = ev.Payload
	case event.EventCheckpoint:
		sseEvent = "checkpoint"
		data = map[string]any{"node_id": ev.NodeID, "state": ev.Payload}
	case event.EventKanbanUpdate:
		sseEvent = "kanban_update"
		data = ev.Payload
	case "kanban.task.submitted", "kanban.task.claimed", "kanban.task.completed", "kanban.task.failed",
		"kanban.claim.timeout":
		sseEvent = "kanban_update"
		data = map[string]any{"event_type": string(ev.Type), "payload": ev.Payload}
	case event.EventApprovalRequired:
		sseEvent = "approval_required"
		data = withNodeID(ev.NodeID, ev.Payload)
	case event.EventGraphEnd:
		sseEvent = "graph_end"
		data = ev.Payload
	case event.EventGraphChanged:
		sseEvent = "graph_changed"
		data = ev.Payload
	case event.EventAgentConfigChanged:
		sseEvent = "agent_config_changed"
		data = ev.Payload
	case event.EventCompileResult:
		sseEvent = "compile_result"
		data = ev.Payload

	case event.EventType(kanban.EventCallbackStart):
		p, ok := ev.Payload.(kanban.CallbackStartPayload)
		if !ok {
			return MappedEvent{}, false
		}
		return MappedEvent{
			Type: "callback_start",
			Payload: injectCommon(map[string]any{
				"card_id":    p.CardID,
				"runtime_id": p.RuntimeID,
				"agent_id":   p.AgentID,
				"query":      p.Query,
			}, ev),
		}, true

	case event.EventType(kanban.EventCallbackDone):
		p, ok := ev.Payload.(kanban.CallbackDonePayload)
		if !ok {
			return MappedEvent{}, false
		}
		payload := map[string]any{
			"card_id":    p.CardID,
			"runtime_id": p.RuntimeID,
			"agent_id":   p.AgentID,
		}
		if p.Error != "" {
			payload["error"] = p.Error
		}
		return MappedEvent{
			Type:    "callback_done",
			Payload: injectCommon(payload, ev),
		}, true

	default:
		return MappedEvent{}, false
	}

	return MappedEvent{
		Type:    sseEvent,
		Payload: injectCommon(data, ev),
	}, true
}

func mapStreamDelta(ev event.Event) (MappedEvent, bool) {
	delta, ok := ev.Payload.(map[string]any)
	if !ok {
		return MappedEvent{}, false
	}
	var sseEvent string
	var data map[string]any
	switch delta["type"] {
	case "token":
		sseEvent = "agent_token"
		data = map[string]any{"node_id": ev.NodeID, "chunk": delta["content"]}
	case "tool_call":
		sseEvent = "agent_tool_call"
		data = map[string]any{"node_id": ev.NodeID, "tool_call_id": delta["id"], "tool_name": delta["name"], "tool_args": delta["arguments"]}
	case "tool_result":
		sseEvent = "agent_tool_result"
		data = map[string]any{
			"node_id": ev.NodeID, "tool_call_id": delta["tool_call_id"],
			"tool_name": delta["name"], "tool_result": delta["content"], "is_error": delta["is_error"],
		}
	default:
		return MappedEvent{}, false
	}
	return MappedEvent{
		Type:    sseEvent,
		Payload: injectCommon(data, ev),
	}, true
}

func injectCommon(data any, ev event.Event) map[string]any {
	m := make(map[string]any)
	if dm, ok := data.(map[string]any); ok {
		for k, v := range dm {
			m[k] = v
		}
	} else if data != nil {
		raw, _ := json.Marshal(data)
		_ = json.Unmarshal(raw, &m)
	}
	m["run_id"] = ev.RunID
	m["graph_id"] = ev.GraphID
	m["timestamp"] = ev.Timestamp.Format(time.RFC3339Nano)
	return m
}

func withNodeID(nodeID string, payload any) map[string]any {
	m := make(map[string]any)
	if pm, ok := payload.(map[string]any); ok {
		for k, v := range pm {
			m[k] = v
		}
	}
	m["node_id"] = nodeID
	return m
}
