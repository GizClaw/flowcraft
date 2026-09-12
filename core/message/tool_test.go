package message_test

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// A result carries parts, never a flattened string, so a payload that
// puts a string (or nothing) in content is rejected instead of being
// silently reinterpreted as a text part.
func TestToolResultRejectsStringContent(t *testing.T) {
	for name, payload := range map[string]string{
		"string content":  `{"call_id":"c1","content":"found","is_error":true}`,
		"empty string":    `{"call_id":"c1","content":""}`,
		"null content":    `{"call_id":"c1","content":null}`,
		"missing content": `{"call_id":"c1"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var result message.ToolResult
			if err := json.Unmarshal([]byte(payload), &result); err != nil {
				return
			}
			if err := result.Validate(); err == nil {
				t.Fatalf("payload %s decoded into a valid result, want rejection", payload)
			}
		})
	}
}

func TestToolResultRoundTripsMultimodalContent(t *testing.T) {
	source, err := media.NewImageBytes([]byte{1, 2, 3}, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	result := message.NewToolResult("c1", message.Content{Parts: []message.Part{
		message.TextPart{Text: "screenshot"},
		message.ImagePart{Source: source},
	}})

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded message.ToolResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(decoded.Content.Parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(decoded.Content.Parts))
	}
	if _, ok := decoded.Content.Parts[1].(message.ImagePart); !ok {
		t.Fatalf("part 1 = %T, want message.ImagePart", decoded.Content.Parts[1])
	}
}
