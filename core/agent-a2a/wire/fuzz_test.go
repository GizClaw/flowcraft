package wire_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-a2a/wire"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// FuzzFromMessage: an arbitrary wire message must either be refused or produce
// a core message the outbound side can render again.
func FuzzFromMessage(f *testing.F) {
	f.Add("hello", "text/plain", "")
	f.Add("", "application/json", `{"k":1}`)
	f.Add("bytes", "image/png", "")
	f.Fuzz(func(t *testing.T, text, mediaType, metadata string) {
		parts := []*a2a.Part{a2a.NewTextPart(text)}
		if mediaType != "" {
			parts = append(parts, &a2a.Part{Content: a2a.Raw([]byte(text)), MediaType: mediaType})
		}
		msg := a2a.NewMessage(a2a.MessageRoleUser, parts...)
		if metadata != "" {
			msg.Metadata = map[string]any{"note": metadata}
		}
		core, err := wire.FromMessage(msg)
		if err != nil {
			return
		}
		if _, err := wire.Message(core); err != nil {
			t.Fatalf("Message(FromMessage(...)) = %v", err)
		}
	})
}
