package agent

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/message"
)

func TestNewAssistantMessagesExcludesSeededTrailingAssistant(t *testing.T) {
	b := NewBoard()
	b.AppendChannelMessage(MainChannel, message.NewTextMessage(message.RoleUser, "seed question"))
	b.AppendChannelMessage(MainChannel, message.NewTextMessage(message.RoleAssistant, "seeded answer"))
	seedLen := len(b.Channel(MainChannel))

	b.AppendChannelMessage(MainChannel, message.NewTextMessage(message.RoleAssistant, "live answer"))

	got := newAssistantMessages(b, seedLen)
	if len(got) != 1 {
		t.Fatalf("messages = %d, want only the live assistant message", len(got))
	}
	if got[0].Content.Text() != "live answer" {
		t.Fatalf("message text = %q, want %q", got[0].Content.Text(), "live answer")
	}
}

func TestNewAssistantMessagesRequiresAppendedAssistant(t *testing.T) {
	b := NewBoard()
	b.AppendChannelMessage(MainChannel, message.NewTextMessage(message.RoleUser, "seed"))
	seedLen := len(b.Channel(MainChannel))
	b.AppendChannelMessage(MainChannel, message.NewTextMessage(message.RoleUser, "interleaved user"))

	if got := newAssistantMessages(b, seedLen); len(got) != 0 {
		t.Fatalf("messages = %d, want none", len(got))
	}
}

func TestNewAssistantMessagesOnlyTrailingBlock(t *testing.T) {
	b := NewBoard()
	seedLen := 0
	b.AppendChannelMessage(MainChannel, message.NewTextMessage(message.RoleAssistant, "first"))
	b.AppendChannelMessage(MainChannel, message.NewTextMessage(message.RoleTool, "result"))
	b.AppendChannelMessage(MainChannel, message.NewTextMessage(message.RoleAssistant, "final"))

	got := newAssistantMessages(b, seedLen)
	if len(got) != 1 || got[0].Content.Text() != "final" {
		t.Fatalf("messages = %+v, want only the trailing assistant message", got)
	}
}
