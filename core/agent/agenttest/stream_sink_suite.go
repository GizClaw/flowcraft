package agenttest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
)

// SinkFactory builds a fresh [agent.StreamSink] for each subtest so
// subtests do not share sink state.
type SinkFactory func() agent.StreamSink

// StreamSinkSuite runs every contract probe that applies to the
// [agent.StreamSink] produced by f:
//
//   - zero-value envelopes and payloads must not panic;
//   - OnDelta MUST observe ctx cancellation and return promptly;
//   - implementations MUST be safe for concurrent OnDelta calls.
func StreamSinkSuite(t *testing.T, f SinkFactory) {
	t.Helper()

	t.Run("ZeroInputNoPanic", func(t *testing.T) { sinkZeroInput(t, f) })
	t.Run("CancelledCtxReturnsPromptly", func(t *testing.T) { sinkCancelledCtx(t, f) })
	t.Run("ConcurrentSafe", func(t *testing.T) { sinkConcurrent(t, f) })
}

// ---------- subtests ----------

func sinkZeroInput(t *testing.T, f SinkFactory) {
	t.Helper()
	s := f()
	defer recoverPanicAs(t, "OnDelta(zero inputs)")
	_ = s.OnDelta(context.Background(), event.Envelope{}, agent.StreamDeltaPayload{})
	_ = s.OnDelta(context.Background(), event.Envelope{}, agent.StreamDeltaPayload{
		Type: agent.StreamDeltaPart,
		Part: message.NewTextMessage(message.RoleAssistant, "hi").Content.Parts[0],
	})
}

func sinkCancelledCtx(t *testing.T, f SinkFactory) {
	t.Helper()
	s := f()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		_ = s.OnDelta(ctx, event.Envelope{}, agent.StreamDeltaPayload{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnDelta did not return within 2s of a cancelled ctx; sinks MUST observe ctx.Done")
	}
}

func sinkConcurrent(t *testing.T, f SinkFactory) {
	t.Helper()
	s := f()
	ctx := context.Background()
	part := message.NewTextMessage(message.RoleAssistant, "hi").Content.Parts[0]
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("concurrent OnDelta panicked: %v", r)
				}
			}()
			_ = s.OnDelta(ctx, event.Envelope{}, agent.StreamDeltaPayload{
				Type: agent.StreamDeltaPart,
				Part: part,
			})
		}()
	}
	wg.Wait()
}
