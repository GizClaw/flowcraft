package commands

import (
	"context"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime/session"

	"github.com/GizClaw/flowcraft/examples/forge/internal/chatfmt"
)

// textCollectorSink accumulates the streamed assistant text for
// scripted turns.
type textCollectorSink struct {
	builder   strings.Builder
	tokens    int
	first     time.Time
	blocks    chatfmt.Collector
	labels    func(nodeID string) string
	callNames map[string]string
}

func (s *textCollectorSink) spec() session.SinkSpec {
	return session.SinkSpec{
		ID: "collector",
		Sink: agent.StreamSinkFunc(func(
			_ context.Context,
			env event.Envelope,
			delta agent.StreamDeltaPayload,
		) error {
			if delta.Type != agent.StreamDeltaPart {
				return nil
			}
			if s.callNames == nil {
				s.callNames = make(map[string]string)
			}
			switch part := delta.Part.(type) {
			case message.TextPart:
				if s.tokens == 0 {
					s.first = time.Now()
				}
				s.tokens++
				s.builder.WriteString(part.Text)
				s.blocks.Token(env.NodeID(), part.Text)
			case message.ToolCallPart:
				s.callNames[part.Call.ID] = part.Call.Name
				s.blocks.ToolCall(part.Call.Name, string(part.Call.Arguments))
			case message.ToolResultPart:
				name := s.callNames[part.Result.CallID]
				if name == "" {
					name = part.Result.CallID
				}
				s.blocks.ToolResult(name, part.Result.Content)
			}
			return nil
		}),
	}
}

// rendered returns the speaker-labelled block rendering for logs.
func (s *textCollectorSink) rendered() string {
	return chatfmt.Render(s.blocks.Blocks, s.labels)
}
