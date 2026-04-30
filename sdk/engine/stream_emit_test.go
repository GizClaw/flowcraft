package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/event"
)

// capturePublisher records every envelope it receives; tests inspect the
// captured slice to assert the helper produced a well-formed delta.
type capturePublisher struct {
	got []event.Envelope
}

func (c *capturePublisher) Publish(_ context.Context, env event.Envelope) error {
	c.got = append(c.got, env)
	return nil
}

func TestEmitStreamToken_HappyPath(t *testing.T) {
	t.Parallel()
	pub := &capturePublisher{}
	if err := EmitStreamToken(context.Background(), pub, "run-1", "node-A", "hello"); err != nil {
		t.Fatalf("EmitStreamToken: %v", err)
	}
	if len(pub.got) != 1 {
		t.Fatalf("publish count = %d, want 1", len(pub.got))
	}
	env := pub.got[0]
	if env.Subject != SubjectStreamDelta("run-1", "node-A") {
		t.Fatalf("subject = %s", env.Subject)
	}
	if env.Headers[event.HeaderRunID] != "run-1" {
		t.Fatalf("HeaderRunID missing: %v", env.Headers)
	}
	if env.Headers[event.HeaderActorID] != "node-A" || env.Headers[event.HeaderNodeID] != "node-A" {
		t.Fatalf("Actor / Node header mismatch: %v", env.Headers)
	}
	p, err := DecodeStreamDelta(env)
	if err != nil {
		t.Fatalf("DecodeStreamDelta: %v", err)
	}
	if p.Type != StreamDeltaToken || p.Content != "hello" {
		t.Fatalf("payload = %+v", p)
	}
}

func TestEmitStreamToolCall_RequiredFields(t *testing.T) {
	t.Parallel()
	pub := &capturePublisher{}
	if err := EmitStreamToolCall(context.Background(), pub, "r", "n", "", "search", nil); err == nil {
		t.Fatal("expected error for missing ID")
	}
	if err := EmitStreamToolCall(context.Background(), pub, "r", "n", "tc-1", "", nil); err == nil {
		t.Fatal("expected error for missing Name")
	}
	if len(pub.got) != 0 {
		t.Fatalf("malformed deltas leaked through: %d envelopes", len(pub.got))
	}

	if err := EmitStreamToolCall(context.Background(), pub, "r", "n", "tc-1", "search", map[string]any{"q": "go"}); err != nil {
		t.Fatalf("happy path: %v", err)
	}
	p, _ := DecodeStreamDelta(pub.got[0])
	if p.Type != StreamDeltaToolCall || p.ID != "tc-1" || p.Name != "search" {
		t.Fatalf("payload = %+v", p)
	}
}

func TestEmitStreamToolResult_RequiredFields(t *testing.T) {
	t.Parallel()
	pub := &capturePublisher{}
	if err := EmitStreamToolResult(context.Background(), pub, "r", "n", "", "search", "x", false, false); err == nil {
		t.Fatal("expected error for missing ToolCallID")
	}

	if err := EmitStreamToolResult(context.Background(), pub, "r", "n", "tc-1", "search", `{"a":1}`, true, false); err != nil {
		t.Fatalf("happy path: %v", err)
	}
	p, _ := DecodeStreamDelta(pub.got[0])
	if p.Type != StreamDeltaToolResult || p.ToolCallID != "tc-1" || !p.IsError {
		t.Fatalf("payload = %+v", p)
	}
}

func TestEmitStreamDelta_NilPublisher(t *testing.T) {
	t.Parallel()
	if err := EmitStreamToken(context.Background(), nil, "r", "n", "x"); err != nil {
		t.Fatalf("nil publisher must be a no-op, got %v", err)
	}
}

func TestEmitStreamDelta_RejectsEmptyType(t *testing.T) {
	t.Parallel()
	pub := &capturePublisher{}
	err := EmitStreamDelta(context.Background(), pub, "r", "n", StreamDeltaPayload{Content: "x"})
	if err == nil || !errors.Is(err, err) || len(pub.got) != 0 {
		t.Fatalf("empty Type must error, got err=%v, got=%d", err, len(pub.got))
	}
}

func TestEmitStreamDelta_AcceptsForwardCompatibleType(t *testing.T) {
	t.Parallel()
	pub := &capturePublisher{}
	if err := EmitStreamDelta(context.Background(), pub, "r", "n", StreamDeltaPayload{Type: "future"}); err != nil {
		t.Fatalf("forward-compat Type must be accepted, got %v", err)
	}
	if len(pub.got) != 1 {
		t.Fatalf("expected publish, got %d", len(pub.got))
	}
}
