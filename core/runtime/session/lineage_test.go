package session

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// TestDelegatedTurnLineageOnSessionEnvelopes verifies the session-level
// mint points (the logical run end) carry the turn's delegation lineage,
// so every envelope a sink observes preserves the run tree. The engine's
// own run-end is consumed by the coordinator as an attempt delimiter, so
// without this stamp the sink would see a lineage-free terminal event.
func TestDelegatedTurnLineageOnSessionEnvelopes(t *testing.T) {
	received := make(chan event.Envelope, 16)
	sink := agent.StreamSinkFunc(func(
		_ context.Context,
		env event.Envelope,
		_ agent.StreamDeltaPayload,
	) error {
		received <- env
		return nil
	})
	engine := agent.EngineFunc(func(
		ctx context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "done"))
		return board, nil
	})
	_, sess, _, _ := newTurnSession(t, engine, turnHostFactory)
	turn, err := sess.StartWithOptions(context.Background(), agent.Request{
		ParentRunID: "caller-run",
		Attributes:  map[string]string{telemetry.AttrToolCallID: "call-delegate-1"},
		Message:     message.NewTextMessage(message.RoleUser, "hi"),
	}, WithSinks(SinkSpec{ID: "s1", Sink: sink}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case env := <-received:
			if env.Subject != agent.SubjectRunEnd(turn.RunID()) {
				continue
			}
			if env.ParentRunID() != "caller-run" || env.ToolCallID() != "call-delegate-1" {
				t.Fatalf("run-end lineage headers = %+v", env.Headers)
			}
			return
		case <-deadline:
			t.Fatal("lineage-stamped run-end envelope never reached the sink")
		}
	}
}

// TestTopLevelTurnStaysHeaderFreeOnSessionEnvelopes guards the inverse:
// a turn started without lineage must not stamp empty parent/tool-call
// headers on the session-level run end.
func TestTopLevelTurnStaysHeaderFreeOnSessionEnvelopes(t *testing.T) {
	received := make(chan event.Envelope, 16)
	sink := agent.StreamSinkFunc(func(
		_ context.Context,
		env event.Envelope,
		_ agent.StreamDeltaPayload,
	) error {
		received <- env
		return nil
	})
	engine := agent.EngineFunc(func(
		ctx context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "done"))
		return board, nil
	})
	_, sess, _, _ := newTurnSession(t, engine, turnHostFactory)
	turn, err := sess.StartWithOptions(context.Background(), agent.Request{
		Message: message.NewTextMessage(message.RoleUser, "hi"),
	}, WithSinks(SinkSpec{ID: "s1", Sink: sink}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case env := <-received:
			if env.Subject != agent.SubjectRunEnd(turn.RunID()) {
				continue
			}
			if env.ParentRunID() != "" || env.ToolCallID() != "" {
				t.Fatalf("top-level run-end lineage headers = %+v, want none", env.Headers)
			}
			return
		case <-deadline:
			t.Fatal("run-end envelope never reached the sink")
		}
	}
}
