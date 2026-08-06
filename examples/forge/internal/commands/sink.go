package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/examples/forge/internal/chatfmt"
	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
)

// textCollectorSink accumulates the streamed assistant text for
// scripted turns.
type textCollectorSink struct {
	builder strings.Builder
	tokens  int
	first   time.Time
	blocks  chatfmt.Collector
	labels  func(nodeID string) string
}

func (s *textCollectorSink) spec() session.SinkSpec {
	return session.SinkSpec{
		ID: "collector",
		Sink: agent.StreamSinkFunc(func(_ context.Context, env event.Envelope, delta agent.StreamDeltaPayload) error {
			switch delta.Type {
			case agent.StreamDeltaToken:
				if s.tokens == 0 {
					s.first = time.Now()
				}
				s.tokens++
				s.builder.WriteString(delta.Content)
				s.blocks.Token(env.NodeID(), delta.Content)
			case agent.StreamDeltaToolCall:
				s.blocks.ToolCall(delta.Name, fmt.Sprint(delta.Arguments))
			case agent.StreamDeltaToolResult:
				s.blocks.ToolResult(delta.Name, delta.Content)
			}
			return nil
		}),
	}
}

// rendered returns the speaker-labelled block rendering for logs.
func (s *textCollectorSink) rendered() string {
	return chatfmt.Render(s.blocks.Blocks, s.labels)
}
