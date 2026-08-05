package commands

import (
	"context"
	"io"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
)

// terminalSink streams token deltas straight to w.
func terminalSink(w io.Writer) session.SinkSpec {
	return session.SinkSpec{
		ID: "terminal",
		Sink: agent.StreamSinkFunc(func(_ context.Context, _ event.Envelope, delta agent.StreamDeltaPayload) error {
			switch delta.Type {
			case agent.StreamDeltaToken:
				_, err := io.WriteString(w, delta.Content)
				return err
			case agent.StreamDeltaToolCall:
				_, err := io.WriteString(w, "\n[tool call: "+delta.Name+"]\n")
				return err
			case agent.StreamDeltaToolResult:
				if text := strings.TrimSpace(delta.Content); text != "" {
					_, err := io.WriteString(w, "\n[tool result: "+text+"]\n")
					return err
				}
			}
			return nil
		}),
	}
}

// textCollectorSink accumulates the streamed assistant text for
// scripted turns.
type textCollectorSink struct {
	builder strings.Builder
	tokens  int
}

func (s *textCollectorSink) spec() session.SinkSpec {
	return session.SinkSpec{
		ID: "collector",
		Sink: agent.StreamSinkFunc(func(_ context.Context, _ event.Envelope, delta agent.StreamDeltaPayload) error {
			if delta.Type == agent.StreamDeltaToken {
				s.tokens++
				s.builder.WriteString(delta.Content)
			}
			return nil
		}),
	}
}
