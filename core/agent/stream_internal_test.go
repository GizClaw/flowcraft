package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
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

func TestEmitStreamPart_HappyPath(t *testing.T) {
	t.Parallel()
	pub := &capturePublisher{}
	const stepActor = "agent-A.node.node-1"
	if err := EmitStreamPart(context.Background(), pub, "run-1", stepActor,
		message.TextPart{Text: "hello"}); err != nil {
		t.Fatalf("EmitStreamPart: %v", err)
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
	// HeaderAgentID / HeaderNodeID respectively.
	if got := env.Headers[event.HeaderAgentID]; got != "agent-A" {
		t.Errorf("HeaderAgentID = %q, want agent-A", got)
	}
	if got := env.Headers[event.HeaderNodeID]; got != "node-1" {
		t.Errorf("HeaderNodeID = %q, want node-1", got)
	}
	p, err := DecodeStreamDelta(env)
	if err != nil {
		t.Fatalf("DecodeStreamDelta: %v", err)
	}
	if p.Type != StreamDeltaPart {
		t.Fatalf("Type = %q, want part", p.Type)
	}
	text, ok := p.Part.(message.TextPart)
	if !ok || text.Text != "hello" {
		t.Fatalf("Part = %#v, want TextPart hello", p.Part)
	}
}

func TestEmitStreamPart_MultimodalPart(t *testing.T) {
	t.Parallel()
	pub := &capturePublisher{}
	source, err := media.NewImageURL("https://img.example.com/a.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	if err := EmitStreamPart(context.Background(), pub, "run-1", "agent-A.node.node-1",
		message.ImagePart{Source: source}); err != nil {
		t.Fatalf("EmitStreamPart: %v", err)
	}
	p, err := DecodeStreamDelta(pub.got[0])
	if err != nil {
		t.Fatalf("DecodeStreamDelta: %v", err)
	}
	image, ok := p.Part.(message.ImagePart)
	if !ok || image.Source.URL() != "https://img.example.com/a.png" {
		t.Fatalf("Part = %#v, want ImagePart with URL source", p.Part)
	}
}

// TestEmitStreamDelta_StepActorWithoutNodeSuffix documents that
// engines whose step convention is NOT graph-runner-shaped (e.g.
// an iterative engine's "<agent>.iter<N>") still get HeaderAgentID
// populated correctly — splitStepActor returns the whole stepActor
// as the agent.id prefix when no ".node." marker is present, and
// leaves HeaderNodeID unset so the header doesn't accidentally
// claim a node id that does not exist.
func TestEmitStreamDelta_StepActorWithoutNodeSuffix(t *testing.T) {
	t.Parallel()
	pub := &capturePublisher{}
	if err := EmitStreamDelta(context.Background(), pub, "r", "agent-A.iter3",
		StreamDeltaPayload{Type: StreamDeltaPart, Part: message.TextPart{Text: "x"}}); err != nil {
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

func TestEmitStreamPart_RejectsNilPart(t *testing.T) {
	t.Parallel()
	pub := &capturePublisher{}
	if err := EmitStreamPart(context.Background(), pub, "r", "n", nil); err == nil {
		t.Fatal("expected error for nil part")
	}
	if len(pub.got) != 0 {
		t.Fatalf("malformed delta leaked through: %d envelopes", len(pub.got))
	}
}

func TestEmitStreamDelta_NilPublisher(t *testing.T) {
	t.Parallel()
	if err := EmitStreamPart(context.Background(), nil, "r", "n",
		message.TextPart{Text: "x"}); err != nil {
		t.Fatalf("nil publisher must be a no-op, got %v", err)
	}
}

func TestEmitStreamDelta_RejectsEmptyType(t *testing.T) {
	t.Parallel()
	pub := &capturePublisher{}
	err := EmitStreamDelta(context.Background(), pub, "r", "n",
		StreamDeltaPayload{Part: message.TextPart{Text: "x"}})
	if err == nil || !errors.Is(err, err) || len(pub.got) != 0 {
		t.Fatalf("empty Type must error, got err=%v, got=%d", err, len(pub.got))
	}
}

func TestEmitStreamDelta_ParallelBranchControlRequiredFields(t *testing.T) {
	t.Parallel()
	pub := &capturePublisher{}

	if err := EmitStreamDelta(context.Background(), pub, "r", "branch-a", StreamDeltaPayload{
		Type:     StreamDeltaParallelBranchAccept,
		BranchID: "branch-a",
	}); err == nil {
		t.Fatal("expected error for missing ForkID")
	}
	if err := EmitStreamDelta(context.Background(), pub, "r", "branch-a", StreamDeltaPayload{
		Type:   StreamDeltaParallelBranchCancel,
		ForkID: "r:start",
		Reason: "intent rejected",
	}); err == nil {
		t.Fatal("expected error for missing BranchID")
	}
	if err := EmitStreamDelta(context.Background(), pub, "r", "branch-a", StreamDeltaPayload{
		Type:     StreamDeltaParallelBranchAccept,
		ForkID:   "r:start",
		BranchID: "branch-a",
		Part:     message.TextPart{Text: "x"},
	}); err == nil {
		t.Fatal("expected error for parallel control carrying Part")
	}
	if len(pub.got) != 0 {
		t.Fatalf("malformed parallel control deltas leaked through: %d envelopes", len(pub.got))
	}

	if err := EmitStreamDelta(context.Background(), pub, "r", "branch-a", StreamDeltaPayload{
		Type:        StreamDeltaParallelBranchCancel,
		ForkID:      "r:start",
		BranchID:    "branch-a",
		Reason:      "intent rejected",
		Speculative: true,
	}); err != nil {
		t.Fatalf("valid parallel control delta: %v", err)
	}
	got, err := DecodeStreamDelta(pub.got[0])
	if err != nil {
		t.Fatalf("DecodeStreamDelta: %v", err)
	}
	if got.Type != StreamDeltaParallelBranchCancel ||
		got.ForkID != "r:start" ||
		got.BranchID != "branch-a" ||
		got.Reason != "intent rejected" ||
		!got.Speculative {
		t.Fatalf("payload = %+v", got)
	}
}

func TestEmitStreamDelta_SpeculativeDataRequiresCompleteBranchIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload StreamDeltaPayload
	}{
		{
			name: "speculative part missing identity",
			payload: StreamDeltaPayload{
				Type:        StreamDeltaPart,
				Part:        message.TextPart{Text: "x"},
				Speculative: true,
			},
		},
		{
			name: "speculative part missing branch",
			payload: StreamDeltaPayload{
				Type:        StreamDeltaPart,
				Part:        message.TextPart{Text: "x"},
				Speculative: true,
				ForkID:      "fork-1",
			},
		},
		{
			name: "non speculative part with full identity",
			payload: StreamDeltaPayload{
				Type:     StreamDeltaPart,
				Part:     message.TextPart{Text: "x"},
				ForkID:   "fork-1",
				BranchID: "branch-a",
			},
		},
		{
			name: "non speculative part with partial identity",
			payload: StreamDeltaPayload{
				Type:   StreamDeltaPart,
				Part:   message.TextPart{Text: "x"},
				ForkID: "fork-1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pub := &capturePublisher{}
			err := EmitStreamDelta(context.Background(), pub, "r", "branch-a", tt.payload)
			if err == nil {
				t.Fatal("expected invalid speculative identity to be rejected")
			}
			if len(pub.got) != 0 {
				t.Fatalf("invalid delta leaked through: %d envelopes", len(pub.got))
			}
		})
	}

	for _, payload := range []StreamDeltaPayload{
		{
			Type:        StreamDeltaPart,
			Part:        message.TextPart{Text: "x"},
			Speculative: true,
			ForkID:      "fork-1",
			BranchID:    "branch-a",
		},
		{Type: StreamDeltaPart, Part: message.TextPart{Text: "x"}},
	} {
		pub := &capturePublisher{}
		if err := EmitStreamDelta(context.Background(), pub, "r", "branch-a", payload); err != nil {
			t.Fatalf("valid payload %+v rejected: %v", payload, err)
		}
		if len(pub.got) != 1 {
			t.Fatalf("valid payload %+v published %d envelopes", payload, len(pub.got))
		}
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

func TestEmitStreamDelta_FinishAndProviderOutputs(t *testing.T) {
	t.Parallel()
	pub := &capturePublisher{}

	if err := EmitStreamDelta(context.Background(), pub, "r", "n",
		StreamDeltaPayload{Type: StreamDeltaFinish}); err == nil {
		t.Fatal("expected error for finish without FinishReason")
	}
	if err := EmitStreamDelta(context.Background(), pub, "r", "n", StreamDeltaPayload{
		Type:         StreamDeltaFinish,
		FinishReason: "completed",
		Part:         message.TextPart{Text: "x"},
	}); err == nil {
		t.Fatal("expected error for finish carrying Part")
	}
	if err := EmitStreamDelta(context.Background(), pub, "r", "n",
		StreamDeltaPayload{Type: StreamDeltaProviderOutputs}); err == nil {
		t.Fatal("expected error for provider_outputs without ProviderOutputs")
	}
	if err := EmitStreamDelta(context.Background(), pub, "r", "n", StreamDeltaPayload{
		Type: StreamDeltaProviderOutputs,
		ProviderOutputs: []ProviderOutputEnvelope{
			{Provider: "fake", Extension: "web_search"},
		},
	}); err == nil {
		t.Fatal("expected error for provider output envelope without Value")
	}
	if len(pub.got) != 0 {
		t.Fatalf("malformed terminal deltas leaked through: %d envelopes", len(pub.got))
	}

	if err := EmitStreamDelta(context.Background(), pub, "r", "n", StreamDeltaPayload{
		Type:         StreamDeltaFinish,
		FinishReason: "completed",
		RequestID:    "req-1",
		ResponseID:   "resp-1",
	}); err != nil {
		t.Fatalf("valid finish delta: %v", err)
	}
	if err := EmitStreamDelta(context.Background(), pub, "r", "n", StreamDeltaPayload{
		Type: StreamDeltaProviderOutputs,
		ProviderOutputs: []ProviderOutputEnvelope{{
			Provider:  "fake",
			Extension: "web_search",
			Value:     json.RawMessage(`{"query":"flowcraft"}`),
		}},
	}); err != nil {
		t.Fatalf("valid provider_outputs delta: %v", err)
	}
	if len(pub.got) != 2 {
		t.Fatalf("expected 2 envelopes, got %d", len(pub.got))
	}

	finish, err := DecodeStreamDelta(pub.got[0])
	if err != nil {
		t.Fatalf("DecodeStreamDelta finish: %v", err)
	}
	if finish.Type != StreamDeltaFinish ||
		finish.FinishReason != "completed" ||
		finish.RequestID != "req-1" || finish.ResponseID != "resp-1" {
		t.Fatalf("finish payload = %+v", finish)
	}

	outputs, err := DecodeStreamDelta(pub.got[1])
	if err != nil {
		t.Fatalf("DecodeStreamDelta provider_outputs: %v", err)
	}
	if outputs.Type != StreamDeltaProviderOutputs ||
		len(outputs.ProviderOutputs) != 1 ||
		outputs.ProviderOutputs[0].Provider != "fake" ||
		outputs.ProviderOutputs[0].Extension != "web_search" ||
		string(outputs.ProviderOutputs[0].Value) != `{"query":"flowcraft"}` {
		t.Fatalf("provider_outputs payload = %+v", outputs)
	}
}
