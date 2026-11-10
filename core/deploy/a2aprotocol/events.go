package a2aprotocol

import (
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-a2a/wire"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// textStream turns a run's text into one artifact per model round.
//
// Text is not a status message: a status carries the latest status, so emitting
// one message per delta would shred the answer across the task history. An
// artifact chunked with Append/LastChunk keeps one artifact per round, which is
// what tasks/get later serves as the complete answer.
//
// One chunk is held back so the round's last chunk can carry LastChunk itself:
// the protocol rejects an artifact update without parts, so closing cannot be a
// separate empty event.
type textStream struct {
	info        a2a.TaskInfoProvider
	artifactURL func(ref string) string
	id          a2a.ArtifactID
	open        bool
	pending     string
}

func newTextStream(info a2a.TaskInfoProvider, artifactURL func(string) string) *textStream {
	return &textStream{info: info, artifactURL: artifactURL}
}

// events maps one run event onto zero or more wire events.
func (s *textStream) events(event agent.Event) []a2a.Event {
	switch typed := event.(type) {
	case agent.StreamStart:
		// A new round starts a new artifact.
		return s.close()
	case agent.Text:
		if typed.Text == "" {
			return nil
		}
		if s.pending == "" {
			s.pending = typed.Text
			return nil
		}
		out := s.chunk(s.pending, false)
		s.pending = typed.Text
		return out
	case agent.ArtifactWritten:
		return []a2a.Event{s.artifact(typed.Artifact)}
	case agent.Status:
		var out []a2a.Event
		if typed.State.Terminal() {
			out = s.close()
		}
		return append(out, a2a.NewStatusUpdateEvent(s.info, wire.State(typed.State), reasonMessage(typed.Reason)))
	default:
		// Tool calls, tool results, usage and reasoning are the agent's working;
		// the protocol has no vocabulary for them.
		return nil
	}
}

// chunk renders one piece of the answer. The first piece of a round opens the
// artifact; later ones append to it.
func (s *textStream) chunk(text string, last bool) []a2a.Event {
	appendChunk := s.open
	if !s.open {
		s.id = a2a.NewArtifactID()
		s.open = true
	}
	return []a2a.Event{&a2a.TaskArtifactUpdateEvent{
		TaskID:    s.info.TaskInfo().TaskID,
		ContextID: s.info.TaskInfo().ContextID,
		Append:    appendChunk,
		LastChunk: last,
		Artifact: &a2a.Artifact{
			ID:    s.id,
			Parts: a2a.ContentParts{a2a.NewTextPart(text)},
		},
	}}
}

// close ends the open artifact, if any, so a client knows the answer is
// complete. The next round starts a fresh artifact.
func (s *textStream) close() []a2a.Event {
	if s.pending == "" {
		return nil
	}
	text := s.pending
	s.pending = ""
	events := s.chunk(text, true)
	s.open = false
	return events
}

// artifact renders one produced artifact: a fetchable URL when the host can
// serve the bytes, the reference itself otherwise.
func (s *textStream) artifact(produced agent.Artifact) a2a.Event {
	artifact := &a2a.Artifact{
		ID:   a2a.ArtifactID(produced.ID),
		Name: produced.Name,
	}
	if artifact.ID == "" {
		artifact.ID = a2a.NewArtifactID()
	}
	if s.artifactURL != nil {
		if url := s.artifactURL(produced.Blob); url != "" {
			artifact.Parts = a2a.ContentParts{
				a2a.NewFileURLPart(a2a.URL(url), produced.MediaType),
			}
		}
	}
	if artifact.Parts == nil {
		artifact.Parts = a2a.ContentParts{a2a.NewDataPart(map[string]string{
			"blob":       produced.Blob,
			"media_type": produced.MediaType,
			"name":       produced.Name,
		})}
	}
	return &a2a.TaskArtifactUpdateEvent{
		TaskID:    s.info.TaskInfo().TaskID,
		ContextID: s.info.TaskInfo().ContextID,
		LastChunk: true,
		Artifact:  artifact,
	}
}

func reasonMessage(reason string) *a2a.Message {
	if reason == "" {
		return nil
	}
	return a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(reason))
}
