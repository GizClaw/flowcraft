package inference

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// TestReasoningScope pins the composition rule: a declared scope replaces the
// derived one entirely (that is how an operator says "these models and
// credentials verify each other"), and otherwise the scope is the address
// that produced the trace, with the credential profile appended only when the
// reference names one.
func TestReasoningScope(t *testing.T) {
	for _, test := range []struct {
		name                               string
		declared, provider, model, profile string
		want                               string
	}{
		{
			name:     "derived from the address",
			provider: "openai", model: "gpt-5.6-luna", profile: "work",
			want: "openai/gpt-5.6-luna/work",
		},
		{
			name:     "no profile in the reference",
			provider: "kimi", model: "kimi-k3",
			want: "kimi/kimi-k3",
		},
		{
			name:     "model switch changes the scope",
			provider: "openai", model: "gpt-5.6-terra", profile: "work",
			want: "openai/gpt-5.6-terra/work",
		},
		{
			name:     "declared scope replaces the address",
			declared: "shared-openai-prod", provider: "openai", model: "gpt-5.6-luna",
			profile: "work",
			want:    "shared-openai-prod",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := ReasoningScope(
				test.declared, test.provider, test.model, test.profile)
			if got != test.want {
				t.Fatalf("ReasoningScope = %q, want %q", got, test.want)
			}
		})
	}
}

// TestWithReasoningSourceStampsParts pins the decode half: every reasoning
// part a decoder produced carries the scope, and nothing else is touched.
func TestWithReasoningSourceStampsParts(t *testing.T) {
	response := GenerateResponse{Message: message.Message{
		Role: message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: "answer"},
			message.ReasoningPart{Text: "thinking", Signature: "sig", ID: "t1"},
			message.ReasoningPart{Signature: "redacted", Source: "stale"},
		}},
	}}
	decode := WithReasoningSource(
		func(context.Context, string) (GenerateResponse, error) {
			return response, nil
		},
		"openai/gpt-5.6-luna/work",
	)
	got, err := decode(context.Background(), "raw")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if text := got.Message.Content.Parts[0].(message.TextPart); text.Text != "answer" {
		t.Fatalf("text part changed: %+v", text)
	}
	for _, index := range []int{1, 2} {
		part, ok := got.Message.Content.Parts[index].(message.ReasoningPart)
		if !ok {
			t.Fatalf("part %d = %T, want ReasoningPart", index, got.Message.Content.Parts[index])
		}
		if part.Source != "openai/gpt-5.6-luna/work" {
			t.Fatalf("part %d source = %q, want the deployment scope", index, part.Source)
		}
	}
	if got.Message.Content.Parts[1].(message.ReasoningPart).Signature != "sig" {
		t.Fatal("stamping rewrote the signature")
	}
}

// TestWithReasoningSourcePassesFailureThrough pins the failure path: a
// decoder error is not an opportunity to rewrite anything.
func TestWithReasoningSourcePassesFailureThrough(t *testing.T) {
	want := context.Canceled
	decode := WithReasoningSource(
		func(context.Context, string) (GenerateResponse, error) {
			return GenerateResponse{}, want
		},
		"scope",
	)
	if _, err := decode(context.Background(), "raw"); err != want {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// TestWithReasoningDeltaSourceStampsBothForms pins the streaming half with the
// delta forms a decoder may return.
func TestWithReasoningDeltaSourceStampsBothForms(t *testing.T) {
	for _, test := range []struct {
		name  string
		delta PartDelta
		read  func(PartDelta) string
	}{
		{
			name:  "value delta",
			delta: ReasoningDelta{Text: "think"},
			read: func(delta PartDelta) string {
				return delta.(ReasoningDelta).Source
			},
		},
		{
			name:  "pointer delta",
			delta: &ReasoningDelta{Text: "think"},
			read: func(delta PartDelta) string {
				return delta.(*ReasoningDelta).Source
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			decode := WithReasoningDeltaSource(
				func(context.Context, string) (GenerateStreamEvent, error) {
					return GenerateStreamEvent{Delta: test.delta}, nil
				},
				"scope",
			)
			event, err := decode(context.Background(), "raw")
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := test.read(event.Delta); got != "scope" {
				t.Fatalf("delta source = %q, want the deployment scope", got)
			}
		})
	}
}

// TestReasoningDeltaAccumulatesSource pins the stream accumulator: the source
// is sticky like the signature, and the assembled part carries it.
func TestReasoningDeltaAccumulatesSource(t *testing.T) {
	accumulator := &generatePartAccumulator{kind: message.PartReasoning}
	for _, delta := range []ReasoningDelta{
		{Text: "thin", Source: "openai/gpt-5.6-luna/work"},
		{Text: "king"},
		{Signature: "sig", ID: "t1", Source: "openai/gpt-5.6-luna/work"},
	} {
		if err := accumulator.add(delta); err != nil {
			t.Fatalf("add(%+v): %v", delta, err)
		}
	}
	part, err := accumulator.result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	reasoning, ok := part.(message.ReasoningPart)
	if !ok {
		t.Fatalf("part = %T, want ReasoningPart", part)
	}
	if reasoning.Text != "thinking" ||
		reasoning.Signature != "sig" ||
		reasoning.ID != "t1" ||
		reasoning.Source != "openai/gpt-5.6-luna/work" {
		t.Fatalf("assembled part = %+v", reasoning)
	}
}

// TestReasoningPartSourceRoundTrips pins the wire form: provenance survives a
// JSON round-trip, so a stored conversation keeps it and a host that rebuilds
// a request from JSON cannot lose the stamp.
func TestReasoningPartSourceRoundTrips(t *testing.T) {
	part := message.ReasoningPart{
		Text:      "thinking",
		Signature: "sig",
		ID:        "t1",
		Source:    "openai/gpt-5.6-luna/work",
	}
	raw, err := message.MarshalPart(part)
	if err != nil {
		t.Fatalf("MarshalPart: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("wire form is not JSON: %v", err)
	}
	if wire["source"] != "openai/gpt-5.6-luna/work" {
		t.Fatalf("wire source = %v, want the scope", wire["source"])
	}
	decoded, err := message.UnmarshalPart(raw)
	if err != nil {
		t.Fatalf("UnmarshalPart: %v", err)
	}
	if decoded != message.Part(part) {
		t.Fatalf("round-trip = %#v, want %#v", decoded, part)
	}
}
