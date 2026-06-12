package claw

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestProviderSafeMessagesMaterializesEmptyUserTextWithoutMutatingInput(t *testing.T) {
	in := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, "system"),
		llm.NewTextMessage(llm.RoleUser, ""),
	}

	out := providerSafeMessages(in)
	if got := out[1].Content(); got != providerSafeEmptyUserText {
		t.Fatalf("provider-safe content = %q, want %q", got, providerSafeEmptyUserText)
	}
	if got := in[1].Content(); got != "" {
		t.Fatalf("input was mutated to %q, want empty", got)
	}
	if &out[0] == &in[0] {
		t.Fatal("providerSafeMessages returned the original backing array after changing a message")
	}
}

func TestProviderSafeMessagesLeavesStructuredUserContentAlone(t *testing.T) {
	in := []llm.Message{{
		Role: llm.RoleUser,
		Parts: []llm.Part{{
			Type: llm.PartData,
			Data: &llm.DataRef{MimeType: "application/json", Value: map[string]any{"ok": true}},
		}},
	}}

	out := providerSafeMessages(in)
	if &out[0] != &in[0] {
		t.Fatal("structured message should keep the original backing array")
	}
	if len(out[0].Parts) != 1 || out[0].Parts[0].Type != model.PartData {
		t.Fatalf("structured message changed: %+v", out[0])
	}
}
