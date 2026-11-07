package a2a

import (
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Update is one A2A stream update.
type Update struct {
	Kind      string    `json:"kind"`
	TaskID    string    `json:"taskId"`
	ContextID string    `json:"contextId,omitempty"`
	Status    *Status   `json:"status,omitempty"`
	Artifact  *Artifact `json:"artifact,omitempty"`
	Append    bool      `json:"append,omitempty"`
	Final     bool      `json:"final"`
}

// Updates maps one agent event onto A2A stream updates and reports whether the
// stream ends with this event.
//
// What crosses the wire and what does not is a deliberate choice:
//
//   - Answer text becomes an appending status update, which is how A2A streams
//     partial output.
//   - Lifecycle states become status updates; a terminal state and
//     input-required both end the stream, because either way the client must act
//     before the task moves again.
//   - An artifact becomes an artifact update carrying its reference.
//   - Tool calls, tool results and token usage do not cross. They are the
//     agent's own working, not a result the client asked for, and A2A has no
//     vocabulary for them. Reasoning is the same: the model's scratch space.
func Updates(taskID, contextID string, event agent.Event) ([]Update, bool, error) {
	switch typed := event.(type) {
	case agent.Text:
		if typed.Text == "" {
			return nil, false, nil
		}
		return []Update{{
			Kind:      "status-update",
			TaskID:    taskID,
			ContextID: contextID,
			Status: &Status{
				State: string(agent.StateWorking),
				Message: &Message{
					Kind:  "message",
					Role:  "agent",
					Parts: []Part{{Kind: "text", Text: typed.Text}},
				},
			},
			Append: true,
		}}, false, nil
	case agent.Status:
		update := Update{
			Kind:      "status-update",
			TaskID:    taskID,
			ContextID: contextID,
			Status:    &Status{State: string(typed.State)},
			Final:     typed.State.Terminal() || typed.State == agent.StateInputRequired,
		}
		if typed.Reason != "" {
			update.Status.Message = &Message{
				Kind:  "message",
				Role:  "agent",
				Parts: []Part{{Kind: "text", Text: typed.Reason}},
			}
		}
		return []Update{update}, update.Final, nil
	case agent.ArtifactWritten:
		return []Update{{
			Kind:      "artifact-update",
			TaskID:    taskID,
			ContextID: contextID,
			Artifact: &Artifact{
				ArtifactID: typed.Artifact.ID,
				Name:       typed.Artifact.Name,
				Parts: []Part{{
					Kind: "file",
					File: &File{
						URI:      typed.Artifact.Blob,
						Name:     typed.Artifact.Name,
						MimeType: typed.Artifact.MediaType,
					},
				}},
			},
		}}, false, nil
	case agent.StreamStart, agent.Reasoning, agent.ToolCallStart, agent.ToolResult, agent.UsageEvent:
		// Internal working; see the doc comment.
		return nil, false, nil
	default:
		// The event set is closed, so an unhandled kind is a new kind that
		// nobody decided the wire form of. Saying so beats dropping it.
		return nil, false, errdefs.Validationf(
			"a2a: event kind %q has no mapping", event.Kind())
	}
}
