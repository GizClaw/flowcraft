package script

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

func TestStringifyPayload(t *testing.T) {
	if got := stringifyPayload("hello"); got != "hello" {
		t.Fatalf("stringifyPayload(string) = %q", got)
	}
	if got := stringifyPayload(map[string]any{"a": 1}); got != `{"a":1}` {
		t.Fatalf("stringifyPayload(map) = %q", got)
	}
}

func TestPayloadToToolCall(t *testing.T) {
	call, ok := payloadToToolCall(map[string]any{
		"id": "c1", "name": "search", "arguments": map[string]any{"q": "go"},
	})
	if !ok {
		t.Fatal("valid tool_call payload rejected")
	}
	if call.ID != "c1" || call.Name != "search" || string(call.Arguments) != `{"q":"go"}` {
		t.Fatalf("call = %+v", call)
	}
	if _, ok := payloadToToolCall(map[string]any{"name": "search"}); ok {
		t.Fatal("missing id must be rejected")
	}
}

func TestPayloadToToolResult(t *testing.T) {
	result, ok := payloadToToolResult(map[string]any{
		"tool_call_id": "c1", "content": "ok", "is_error": false,
	})
	if !ok {
		t.Fatal("valid tool_result payload rejected")
	}
	if result.CallID != "c1" || result.Content != "ok" || result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if _, ok := payloadToToolResult(map[string]any{"content": "x"}); ok {
		t.Fatal("missing tool_call_id must be rejected")
	}
}

func TestPayloadToPart(t *testing.T) {
	part, ok := payloadToPart(map[string]any{"type": "text", "text": "hi"})
	if !ok {
		t.Fatal("valid text part rejected")
	}
	if text, ok := part.(message.TextPart); !ok || text.Text != "hi" {
		t.Fatalf("part = %#v", part)
	}

	part, ok = payloadToPart(`{"type":"image","source":{"kind":"url","url":"https://x/i.png","media_type":"image/png"}}`)
	if !ok {
		t.Fatal("valid image part rejected")
	}
	if _, ok := part.(message.ImagePart); !ok {
		t.Fatalf("part = %T, want ImagePart", part)
	}

	if _, ok := payloadToPart(map[string]any{"type": "hologram"}); ok {
		t.Fatal("unknown part type must be rejected")
	}
	if _, ok := payloadToPart(nil); ok {
		t.Fatal("nil payload must be rejected")
	}
}
