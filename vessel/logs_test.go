package vessel

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// streamingEngine emits a few stream-delta tokens via the host's
// publisher, then appends a final assistant message.
func streamingEngine(tokens ...string) engine.Engine {
	return engine.EngineFunc(func(ctx context.Context, run engine.Run, host engine.Host, b *engine.Board) (*engine.Board, error) {
		for _, tok := range tokens {
			if err := engine.EmitStreamToken(ctx, host, run.ID, "node-A", tok); err != nil {
				return b, err
			}
		}
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "final"))
		return b, nil
	})
}

func TestLogs_DeliversStreamDeltas(t *testing.T) {
	t.Parallel()
	c, err := New(spec.Spec{Agents: []spec.Agent{{Name: "p"}}}, WithEngine(streamingEngine("alpha", "beta", "gamma")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop(context.Background())
	_ = c.Launch(context.Background())

	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := Logs(subCtx, c)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}

	if _, err := c.Call(context.Background(), "p", agent.Request{Message: model.NewTextMessage(model.RoleUser, "go")}); err != nil {
		t.Fatalf("Call: %v", err)
	}

	got := make([]string, 0, 3)
	deadline := time.After(2 * time.Second)
	for len(got) < 3 {
		select {
		case entry, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed early; got=%v", got)
			}
			// Stream-delta envelopes land as Type=stream.delta with
			// the StreamDeltaPayload fields preserved verbatim in
			// Payload (see decodeEnvelopePayload). Pull "type" /
			// "content" via map access — the test's contract is
			// "delta with type=token surfaces with content==token".
			if entry.Type != LogEntryStreamDelta {
				continue
			}
			if pt, _ := entry.Payload["type"].(string); pt != string(engine.StreamDeltaToken) {
				continue
			}
			content, _ := entry.Payload["content"].(string)
			got = append(got, content)
		case <-deadline:
			t.Fatalf("timed out; got=%v", got)
		}
	}
	want := []string{"alpha", "beta", "gamma"}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestLogsForRun_FiltersByRunID(t *testing.T) {
	t.Parallel()
	c, err := New(spec.Spec{Agents: []spec.Agent{{Name: "p"}}}, WithEngine(streamingEngine("hello")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop(context.Background())
	_ = c.Launch(context.Background())

	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Submit a run first so we have a known runID, then start the
	// filter against an UNRELATED id and confirm no entries arrive.
	h, _ := c.Submit(context.Background(), "p", agent.Request{Message: model.NewTextMessage(model.RoleUser, "x")})
	ch, err := LogsForRun(subCtx, c, "run-does-not-exist")
	if err != nil {
		t.Fatalf("LogsForRun: %v", err)
	}
	_, _ = h.Wait(context.Background())

	select {
	case entry, ok := <-ch:
		if ok {
			t.Fatalf("unexpected entry leaked through filter: %+v", entry)
		}
	case <-time.After(150 * time.Millisecond):
		// good — nothing arrived for the bogus runID
	}
}
