package message_test

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// Every provider call marshals one request content and unmarshals one response
// content, so these two are on the model-call path twice per turn. They are also
// where a per-part allocation hides: a message with an image and a tool call is the
// ordinary shape, not the exotic one.

func benchContent() message.Content {
	image, err := media.NewImageBytes([]byte("image-bytes"), "image/png")
	if err != nil {
		panic(err)
	}
	return message.Content{Parts: []message.Part{
		message.TextPart{Text: "summarise this screenshot"},
		message.ImagePart{Source: image},
		message.ToolCallPart{Call: message.ToolCall{
			ID:        "call-1",
			Name:      "search",
			Arguments: message.NewArguments(json.RawMessage(`{"q":"release notes","limit":5}`)),
		}},
		message.ReasoningPart{Text: "the user wants a summary", Signature: "sig"},
	}}
}

func BenchmarkContentMarshal(b *testing.B) {
	content := benchContent()
	if _, err := json.Marshal(content); err != nil {
		b.Fatalf("marshal: %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := json.Marshal(content); err != nil {
			b.Fatalf("marshal: %v", err)
		}
	}
}

func BenchmarkContentUnmarshal(b *testing.B) {
	payload, err := json.Marshal(benchContent())
	if err != nil {
		b.Fatalf("marshal: %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		var content message.Content
		if err := json.Unmarshal(payload, &content); err != nil {
			b.Fatalf("unmarshal: %v", err)
		}
	}
}

// BenchmarkPartRoundTrip is the single-part path a stream delta takes: one part
// encoded and decoded per event.
func BenchmarkPartRoundTrip(b *testing.B) {
	part := message.TextPart{Text: "a delta of text"}
	payload, err := message.MarshalPart(part)
	if err != nil {
		b.Fatalf("MarshalPart: %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := message.MarshalPart(part); err != nil {
			b.Fatalf("MarshalPart: %v", err)
		}
		if _, err := message.UnmarshalPart(payload); err != nil {
			b.Fatalf("UnmarshalPart: %v", err)
		}
	}
}
