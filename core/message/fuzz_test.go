package message_test

import (
	"testing"

	"github.com/goccy/go-json"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// FuzzContentRoundTrip: a message body arrives as bytes — from a provider, a
// checkpoint, a peer over A2A — and every one of those is untrusted.
//
// Two invariants, both of them properties a single test case cannot state:
//
//   - decoding never panics, whatever the bytes are;
//   - anything this package decoded re-encodes, and its own output decodes again.
//     A body that decodes but cannot be written is a transcript that cannot be
//     persisted, which is the failure a hand-written case never reaches because it
//     is always written from the encoder's point of view.
func FuzzContentRoundTrip(f *testing.F) {
	image, err := media.NewImageURL("https://example.com/cat.png", "image/png")
	if err != nil {
		f.Fatalf("NewImageURL: %v", err)
	}
	call, err := message.NewToolCall("c1", "search", map[string]any{"q": "x"})
	if err != nil {
		f.Fatalf("NewToolCall: %v", err)
	}
	for _, seed := range []message.Content{
		message.NewTextContent("hello"),
		{Parts: []message.Part{message.ImagePart{Source: image}}},
		{Parts: []message.Part{&message.ToolCallPart{Call: call}}},
		{Parts: []message.Part{message.ReasoningPart{Text: "why", Signature: "sig"}}},
		{Parts: []message.Part{&message.ToolResultPart{Result: message.NewToolResult("c1", message.NewTextContent("ok"))}}},
		{Parts: []message.Part{
			message.TextPart{Text: "mixed"},
			message.ReasoningPart{Signature: "opaque"},
		}},
	} {
		data, err := json.Marshal(seed)
		if err != nil {
			f.Fatalf("marshal seed: %v", err)
		}
		f.Add(data)
	}
	for _, raw := range []string{
		`{"parts":[{"type":"text"}]}`,
		`{"parts":[{"type":"tool_call"}]}`,
		`{"parts":[{"type":"bogus"}]}`,
		`{"parts":null}`,
		`[]`,
		`null`,
		``,
		`{"parts":[{"type":"text","text":"` + "\xff" + `"}]}`,
	} {
		f.Add([]byte(raw))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var decoded message.Content
		if err := json.Unmarshal(data, &decoded); err != nil {
			return
		}
		// The decoder is deliberately permissive where the encoder is strict: a
		// content with no parts decodes (an empty envelope is a legal wire value)
		// and refuses to encode. The invariant is about what this package promises
		// to be able to write, so it starts where Validate says the value is one.
		if err := decoded.Validate(); err != nil {
			return
		}
		encoded, err := json.Marshal(decoded)
		if err != nil {
			t.Fatalf("content that decoded failed to encode: %v\ninput: %q", err, data)
		}
		var again message.Content
		if err := json.Unmarshal(encoded, &again); err != nil {
			t.Fatalf("Marshal wrote bytes Unmarshal rejects: %v\ninput: %q\nencoded: %q",
				err, data, encoded)
		}
		if len(again.Parts) != len(decoded.Parts) {
			t.Fatalf("round trip changed the part count: %d -> %d\ninput: %q\nencoded: %q",
				len(decoded.Parts), len(again.Parts), data, encoded)
		}
	})
}

// FuzzPartRoundTrip is the same property one part at a time, through the
// single-part entry points a host uses when it projects a stream: a part that
// decodes must encode, and its encoding must decode.
func FuzzPartRoundTrip(f *testing.F) {
	for _, raw := range []string{
		`{"type":"text","text":"hi"}`,
		`{"type":"reasoning","text":"why","signature":"s"}`,
		`{"type":"image","source":{"kind":"url","url":"https://e/x.png","media_type":"image/png"}}`,
		`{"type":"tool_call","call":{"id":"c1","name":"search","arguments":{}}}`,
		`{"type":"tool_result","result":{"call_id":"c1"}}`,
		`{"type":"",""}`,
		`{"type":"text","text":null}`,
		`{"type":"tool_call","call":{"arguments":"not an object"}}`,
		`null`,
		``,
	} {
		f.Add([]byte(raw))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		part, err := message.UnmarshalPart(data)
		if err != nil {
			return
		}
		// Same asymmetry as Content: a tool result whose content is empty decodes
		// and is not a part this package will write. Validate is where that line is.
		if err := part.Validate(); err != nil {
			return
		}
		encoded, err := message.MarshalPart(part)
		if err != nil {
			t.Fatalf("part that decoded failed to encode: %v\ninput: %q", err, data)
		}
		again, err := message.UnmarshalPart(encoded)
		if err != nil {
			t.Fatalf("MarshalPart wrote bytes UnmarshalPart rejects: %v\ninput: %q\nencoded: %q",
				err, data, encoded)
		}
		if again.Kind() != part.Kind() {
			t.Fatalf("round trip changed the kind: %q -> %q\ninput: %q", part.Kind(), again.Kind(), data)
		}
	})
}
