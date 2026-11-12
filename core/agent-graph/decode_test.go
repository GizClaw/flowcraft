package graph

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// A reader outside this package decodes a record with this function, so every
// kind the engine writes must come back as the type that wrote it — and come back
// equal, or a renderer will show a field the log does not hold.
func TestDecodeRecordRoundTripsEveryKind(t *testing.T) {
	cases := []struct {
		kind  string
		value any
	}{
		{RecordFrontier, FrontierRecord{Step: 2, Nodes: []string{"planner", "worker"}, Replay: true}},
		{RecordState, StateWrite{Step: 1, Key: "messages", Value: EncodedValue{
			Kind:     KindMessages,
			Messages: []message.Message{message.NewTextMessage(message.RoleUser, "hi")},
		}}},
		{RecordState, StateWrite{Step: 1, Key: "shot", Value: EncodedValue{
			Kind: KindImage, MediaType: "image/png", Blob: "sha256:blob",
		}}},
		{RecordBase, BaseRecord{Step: 3, Blob: "sha256:base"}},
		{RecordLLMCall, agent.LLMCallRecord{
			RequestHash: "sha256:request",
			Model:       model.ModelRef{ID: model.ModelID{Provider: "p", Name: "requested"}},
			Metadata: inference.Metadata{
				Model:      model.ModelID{Provider: "p", Name: "resolved"},
				ResponseID: "resp-1",
			},
			StopReason:  "stop",
			LatencyMs:   120,
			ResponseRef: "sha256:response",
			Messages:    []agent.MessageDigest{{Role: message.RoleUser, Origin: "user", Chars: 2, Hash: "sha256:m"}},
		}},
		{RecordEffect, agent.EffectRecord{
			CallID: "c-1", ToolName: "read", ArgsHash: "sha256:a",
			IdempotencyKey: "c-1", Status: agent.EffectDone, ResultHash: "sha256:r", DurationMs: 5,
		}},
	}
	for _, tc := range cases {
		payload, err := json.Marshal(tc.value)
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.kind, err)
		}
		decoded, ok := DecodeRecord(agent.EventRecord{Kind: tc.kind, Payload: payload})
		if !ok {
			t.Errorf("%s: not decoded", tc.kind)
			continue
		}
		again, err := json.Marshal(decoded)
		if err != nil {
			t.Fatalf("%s: marshal decoded: %v", tc.kind, err)
		}
		if string(again) != string(payload) {
			t.Errorf("%s round-tripped to a different value:\n in: %s\nout: %s", tc.kind, payload, again)
		}
	}
}

// A kind this engine does not own is not an error: the caller falls back to the
// next decoder, and then to the raw payload.
func TestDecodeRecordIgnoresForeignKinds(t *testing.T) {
	for _, kind := range []string{"script.run", "script.tool", "message", "host.fact", ""} {
		if _, ok := DecodeRecord(agent.EventRecord{Kind: kind, Payload: json.RawMessage(`{}`)}); ok {
			t.Errorf("kind %q was claimed by the graph decoder", kind)
		}
	}
}

// A payload that does not decode reports false rather than a half-filled value:
// the reader must be able to tell "another engine's record" from "mine, broken".
func TestDecodeRecordRefusesABrokenPayload(t *testing.T) {
	for _, kind := range []string{RecordFrontier, RecordState, RecordBase, RecordLLMCall, RecordEffect} {
		if _, ok := DecodeRecord(agent.EventRecord{Kind: kind, Payload: json.RawMessage(`"not an object"`)}); ok {
			t.Errorf("kind %q decoded a payload that is not its own", kind)
		}
	}
}

// The projection is where a reader finds the decision point it is looking at.
func TestDecodeProjection(t *testing.T) {
	projection, err := DecodeProjection(json.RawMessage(
		`{"step":4,"routed":9,"frontier":["a"],"context_hash":"sha256:x"}`))
	if err != nil {
		t.Fatalf("DecodeProjection: %v", err)
	}
	if projection.Step != 4 || projection.ContextHash != "sha256:x" {
		t.Errorf("projection = %+v", projection)
	}
	if _, err := DecodeProjection(json.RawMessage(`[]`)); err == nil {
		t.Error("a projection that is not an object was accepted")
	}
}
