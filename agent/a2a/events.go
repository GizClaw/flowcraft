package a2a

import (
	"github.com/GizClaw/flowcraft/agent/a2a/wire"
	"github.com/GizClaw/flowcraft/core/agent"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// handle maps one wire event onto core events and reports whether the task reached a terminal
// state.
func (d *driver) handle(event a2a.Event, yield func(agent.Event, error) bool) (bool, error) {
	switch typed := event.(type) {
	case *a2a.Task:
		d.adopt(string(typed.ID), typed.ContextID)
		state := wire.FromState(typed.Status.State)
		d.setState(state)
		if !yield(agent.Status{State: state}, nil) {
			return true, nil
		}
		return state.Terminal(), nil
	case *a2a.TaskStatusUpdateEvent:
		d.adopt(string(typed.TaskID), typed.ContextID)
		state := wire.FromState(typed.Status.State)
		d.setState(state)
		// The status message is the status's own, not answer text: A2A puts the question a paused
		// task asks there, and everything that accompanies a state here is a reason. Emitting it as
		// Text would fold a question into the answer, and the caller would have no reason to show.
		if !yield(agent.Status{State: state, Reason: wire.Text(typed.Status.Message)}, nil) {
			return true, nil
		}
		return state.Terminal(), nil
	case *a2a.TaskArtifactUpdateEvent:
		d.adopt(string(typed.TaskID), typed.ContextID)
		if typed.Artifact == nil {
			return false, nil
		}
		d.artifact(typed.Artifact, yield)
		return false, nil
	case *a2a.Message:
		if text := wire.Text(typed); text != "" && !yield(agent.Text{Text: text}, nil) {
			return true, nil
		}
		return false, nil
	default:
		// Tool calls, reasoning and usage have no local counterpart on a proxied task: the peer
		// owns that work, and inventing events for it would report accounting nobody measured.
		return false, nil
	}
}

// adopt keeps the peer's identity as the stream reports it.
func (d *driver) adopt(taskID, contextID string) {
	d.mu.Lock()
	if taskID != "" {
		d.remote.TaskID = taskID
		d.remote.LastTaskID = taskID
	}
	if contextID != "" {
		d.remote.ContextID = contextID
	}
	d.mu.Unlock()
}

// artifact turns one artifact chunk into answer text, or into a produced artifact.
//
// Text arrives chunked — the peer appends one part per delta — so each text part is a delta of the
// answer. Anything else is a reference, reported once for the whole artifact.
func (d *driver) artifact(remote *a2a.Artifact, yield func(agent.Event, error) bool) {
	produced := false
	for _, part := range remote.Parts {
		text, ok := part.Content.(a2a.Text)
		if !ok {
			produced = true
			continue
		}
		if string(text) == "" {
			continue
		}
		if !yield(agent.Text{Text: string(text)}, nil) {
			return
		}
	}
	if produced {
		yield(agent.ArtifactWritten{Artifact: artifactOf(remote)}, nil)
	}
}
