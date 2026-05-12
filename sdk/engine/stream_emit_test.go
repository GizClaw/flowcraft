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
	const stepActor = "agent-A.node.node-1"
	if err := EmitStreamToken(context.Background(), pub, "run-1", stepActor, "hello"); err != nil {
		t.Fatalf("EmitStreamToken: %v", err)
	}
	if len(pub.got) != 1 {
		t.Fatalf("publish count = %d, want 1", len(pub.got))
	}
	env := pub.got[0]
	if env.Subject != SubjectStreamDelta("run-1", stepActor) {
		t.Fatalf("subject = %s", env.Subject)
	}
	if env.Headers[event.HeaderRunID] != "run-1" {
		t.Fatalf("HeaderRunID missing: %v", env.Headers)
	}
	// stepActor is split into the agent.id prefix + the optional
	// ".node.<nodeID>" suffix; the helper projects them onto
	// HeaderAgentID / HeaderNodeID respectively. HeaderActorID is the
	// legacy mirror written by SetAgentID's dual-write.
	if got := env.Headers[event.HeaderAgentID]; got != "agent-A" {
		t.Errorf("HeaderAgentID = %q, want agent-A", got)
	}
	if got := env.Headers[event.HeaderNodeID]; got != "node-1" {
		t.Errorf("HeaderNodeID = %q, want node-1", got)
	}
	if got := env.Headers[event.HeaderActorID]; got != "agent-A" {
		t.Errorf("HeaderActorID (legacy mirror) = %q, want agent-A", got)
	}
	p, err := DecodeStreamDelta(env)
	if err != nil {
		t.Fatalf("DecodeStreamDelta: %v", err)
	}
	if p.Type != StreamDeltaToken || p.Content != "hello" {
		t.Fatalf("payload = %+v", p)
	}
}

// TestEmitStreamDelta_StepActorWithoutNodeSuffix documents that
// engines whose step convention is NOT graph-runner-shaped (e.g.
// vessel inline's "<agent>.iter<N>") still get HeaderAgentID
// populated correctly — splitStepActor returns the whole stepActor
// as the agent.id prefix when no ".node." marker is present, and
// leaves HeaderNodeID unset so the header doesn't accidentally
// claim a node id that does not exist.
func TestEmitStreamDelta_StepActorWithoutNodeSuffix(t *testing.T) {
	t.Parallel()
	pub := &capturePublisher{}
	if err := EmitStreamDelta(context.Background(), pub, "r", "agent-A.iter3",
		StreamDeltaPayload{Type: StreamDeltaToken, Content: "x"}); err != nil {
		t.Fatalf("EmitStreamDelta: %v", err)
	}
	env := pub.got[0]
	if got := env.Headers[event.HeaderAgentID]; got != "agent-A.iter3" {
		t.Errorf("HeaderAgentID = %q, want agent-A.iter3 (no .node. suffix → whole stepActor is prefix)", got)
	}
	if _, ok := env.Headers[event.HeaderNodeID]; ok {
		t.Errorf("HeaderNodeID should be unset when stepActor has no .node. suffix, got %q", env.Headers[event.HeaderNodeID])
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
