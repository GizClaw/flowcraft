package commands

import (
	"context"
	"strings"
	"time"

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
}

func (s *textCollectorSink) spec() session.SinkSpec {
	return session.SinkSpec{
		ID: "collector",
		Sink: agent.StreamSinkFunc(func(_ context.Context, _ event.Envelope, delta agent.StreamDeltaPayload) error {
			if delta.Type == agent.StreamDeltaToken {
				if s.tokens == 0 {
					s.first = time.Now()
				}
				s.tokens++
				s.builder.WriteString(delta.Content)
			}
			return nil
		}),
	}
}
