package history_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/history"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// TestFileStore_SaveMessages_RewritesOnEqualLength pins the
// godoc-aligned contract for issue #153. Pre-fix, FileStore
// short-circuited `len(messages) == len(existing)` to return nil
// without writing — making in-place edits (moderation rewrites,
// redactions, single-message replacements) silently lost.
func TestFileStore_SaveMessages_RewritesOnEqualLength(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := history.NewFileStore(ws, "memory")
	ctx := context.Background()
	conv := "conv-153"

	orig := []model.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "original-A"}}},
		{Role: model.RoleAssistant, Parts: []model.Part{{Type: model.PartText, Text: "original-B"}}},
	}
	if err := store.SaveMessages(ctx, conv, orig); err != nil {
		t.Fatalf("initial SaveMessages: %v", err)
	}

	// In-place edit: same length, different content.
	edited := []model.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "REDACTED-A"}}},
		{Role: model.RoleAssistant, Parts: []model.Part{{Type: model.PartText, Text: "REDACTED-B"}}},
	}
	if err := store.SaveMessages(ctx, conv, edited); err != nil {
		t.Fatalf("edit SaveMessages: %v", err)
	}

	got, err := store.GetMessages(ctx, conv)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	for i, msg := range got {
		text := ""
		if len(msg.Parts) > 0 {
			text = msg.Parts[0].Text
		}
		want := edited[i].Parts[0].Text
		if text != want {
			t.Errorf("#153 regression: msg[%d] = %q, want %q (FileStore.SaveMessages dropped the edit)", i, text, want)
		}
	}
}

func TestFileStore_SaveMessages_RewritesWhenExistingIsNotPrefix(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store := history.NewFileStore(ws, "memory")
	ctx := context.Background()
	conv := "conv-stale-prefix"

	orig := []model.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "stale-A"}}},
		{Role: model.RoleAssistant, Parts: []model.Part{{Type: model.PartText, Text: "stale-B"}}},
	}
	if err := store.SaveMessages(ctx, conv, orig); err != nil {
		t.Fatalf("initial SaveMessages: %v", err)
	}

	replacement := []model.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "fresh-A"}}},
		{Role: model.RoleAssistant, Parts: []model.Part{{Type: model.PartText, Text: "fresh-B"}}},
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "fresh-C"}}},
	}
	if err := store.SaveMessages(ctx, conv, replacement); err != nil {
		t.Fatalf("replacement SaveMessages: %v", err)
	}

	got, err := store.GetMessages(ctx, conv)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(got) != len(replacement) {
		t.Fatalf("len = %d, want %d", len(got), len(replacement))
	}
	for i := range replacement {
		if got[i].Parts[0].Text != replacement[i].Parts[0].Text {
			t.Fatalf("msg[%d] = %q, want %q", i, got[i].Parts[0].Text, replacement[i].Parts[0].Text)
		}
	}
}

// Use llm package once to avoid unused-import warning in alternate
// build configurations. The history.Store contract uses model.Message
// directly; the llm symbol below is only retained as a sanity probe.
var _ = llm.RoleUser
