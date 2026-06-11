package recent

import (
	"context"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestWindow_Descriptor(t *testing.T) {
	store := message.NewWorkspaceStore(newTestWorkspace())
	view := NewWindow(store, WithID("chat-window"), WithVersion("v-test"))

	got := view.Descriptor()
	want := views.Descriptor{
		ID:      "chat-window",
		Kind:    views.KindRecentWindow,
		Version: "v-test",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Descriptor = %#v, want %#v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Descriptor validation failed: %v", err)
	}
}

func TestWindow_LoadUsesDefaultBudgetForRecentTail(t *testing.T) {
	ctx := context.Background()
	store := message.NewWorkspaceStore(newTestWorkspace())
	appended := appendMessages(t, ctx, store, "conv-1", "one", "two", "three", "four", "five")
	view := NewWindow(store, WithDefaultBudget(WindowBudget{MaxMessages: 2}))

	got, err := view.Load(ctx, windowRequest("conv-1"))
	if err != nil {
		t.Fatal(err)
	}

	assertMessageTexts(t, got.Messages, []string{"four", "five"})
	assertMessageSeqs(t, got.Messages, []uint64{4, 5})
	if !got.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	if !reflect.DeepEqual(got.Descriptor, view.Descriptor()) {
		t.Fatalf("Result descriptor = %#v, want view descriptor", got.Descriptor)
	}
	assertSourceRefs(t, got.SourceRefs, appended[3:])
}

func TestWindow_LoadAfterSeqUsesForwardWindow(t *testing.T) {
	ctx := context.Background()
	store := message.NewWorkspaceStore(newTestWorkspace())
	appended := appendMessages(t, ctx, store, "conv-1", "one", "two", "three", "four", "five")
	view := NewWindow(store)

	got, err := view.Load(ctx, WindowRequest{
		Scope:    testScope("conv-1"),
		AfterSeq: 2,
		Budget:   WindowBudget{MaxMessages: 2},
	})
	if err != nil {
		t.Fatal(err)
	}

	assertMessageTexts(t, got.Messages, []string{"three", "four"})
	assertMessageSeqs(t, got.Messages, []uint64{3, 4})
	if !got.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	assertSourceRefs(t, got.SourceRefs, appended[2:4])
}

func TestWindow_LoadEmptyConversationReturnsEmptyResult(t *testing.T) {
	ctx := context.Background()
	store := message.NewWorkspaceStore(newTestWorkspace())
	view := NewWindow(store, WithDefaultBudget(WindowBudget{MaxMessages: 3}))

	got, err := view.Load(ctx, windowRequest("missing-conv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 0 {
		t.Fatalf("Messages len = %d, want 0", len(got.Messages))
	}
	if len(got.SourceRefs) != 0 {
		t.Fatalf("SourceRefs len = %d, want 0", len(got.SourceRefs))
	}
	if got.Truncated {
		t.Fatal("Truncated = true, want false")
	}
}

func TestWindow_LoadRequiresConversationID(t *testing.T) {
	ctx := context.Background()
	store := message.NewWorkspaceStore(newTestWorkspace())
	view := NewWindow(store)

	_, err := view.Load(ctx, WindowRequest{})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Load err = %v, want validation error", err)
	}
}

func TestWindow_LoadZeroBudgetReturnsAllMessages(t *testing.T) {
	ctx := context.Background()
	store := message.NewWorkspaceStore(newTestWorkspace())
	appended := appendMessages(t, ctx, store, "conv-1", "one", "two", "three")
	view := NewWindow(store)

	got, err := view.Load(ctx, windowRequest("conv-1"))
	if err != nil {
		t.Fatal(err)
	}

	assertMessageTexts(t, got.Messages, []string{"one", "two", "three"})
	if got.Truncated {
		t.Fatal("Truncated = true, want false")
	}
	assertSourceRefs(t, got.SourceRefs, appended)
}

func TestWindow_LoadNegativeBudgetOverridesDefaultWithNoLimit(t *testing.T) {
	ctx := context.Background()
	store := message.NewWorkspaceStore(newTestWorkspace())
	appendMessages(t, ctx, store, "conv-1", "one", "two", "three")
	view := NewWindow(store, WithDefaultBudget(WindowBudget{MaxMessages: 1}))

	got, err := view.Load(ctx, WindowRequest{
		Scope:  testScope("conv-1"),
		Budget: WindowBudget{MaxMessages: -1},
	})
	if err != nil {
		t.Fatal(err)
	}

	assertMessageTexts(t, got.Messages, []string{"one", "two", "three"})
	if got.Truncated {
		t.Fatal("Truncated = true, want false")
	}
}

func newTestWorkspace() workspace.Workspace {
	root := workspace.NewMemWorkspace()
	return workspace.Sub(root, "memory/views/recent/window-test")
}

func windowRequest(conversationID string) WindowRequest {
	return WindowRequest{Scope: testScope(conversationID)}
}

func testScope(conversationID string) views.Scope {
	return views.Scope{RuntimeID: "runtime-1", UserID: "user-1", ConversationID: conversationID}
}

func appendMessages(t *testing.T, ctx context.Context, store message.Store, conversationID string, texts ...string) []message.Message {
	t.Helper()

	messages := make([]message.Message, 0, len(texts))
	for _, text := range texts {
		messages = append(messages, message.Message{Message: textPayload(model.RoleUser, text)})
	}
	appended, err := store.Append(ctx, message.AppendRequest{
		ConversationID: conversationID,
		Messages:       messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	return appended
}

func textPayload(role model.Role, text string) model.Message {
	return model.Message{
		Role: role,
		Parts: []model.Part{
			{Type: model.PartText, Text: text},
		},
	}
}

func assertMessageTexts(t *testing.T, messages []message.Message, want []string) {
	t.Helper()

	got := make([]string, 0, len(messages))
	for _, msg := range messages {
		if len(msg.Parts) != 1 {
			t.Fatalf("message %q has %d parts, want 1", msg.ID, len(msg.Parts))
		}
		got = append(got, msg.Parts[0].Text)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("message texts = %v, want %v", got, want)
	}
}

func assertMessageSeqs(t *testing.T, messages []message.Message, want []uint64) {
	t.Helper()

	got := make([]uint64, 0, len(messages))
	for _, msg := range messages {
		got = append(got, msg.Seq)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("message seqs = %v, want %v", got, want)
	}
}

func assertSourceRefs(t *testing.T, got []views.SourceRef, messages []message.Message) {
	t.Helper()

	if len(got) != len(messages) {
		t.Fatalf("SourceRefs len = %d, want %d", len(got), len(messages))
	}
	for i, ref := range got {
		if err := ref.Validate(); err != nil {
			t.Fatalf("SourceRefs[%d] validation failed: %v", i, err)
		}
		if ref.Kind != views.SourceMessage {
			t.Fatalf("SourceRefs[%d].Kind = %q, want message", i, ref.Kind)
		}
		if ref.Message == nil {
			t.Fatalf("SourceRefs[%d].Message = nil, want payload", i)
		}
		if ref.Message.ConversationID != messages[i].ConversationID {
			t.Fatalf("SourceRefs[%d].ConversationID = %q, want %q", i, ref.Message.ConversationID, messages[i].ConversationID)
		}
		if ref.Message.MessageID != messages[i].ID {
			t.Fatalf("SourceRefs[%d].MessageID = %q, want %q", i, ref.Message.MessageID, messages[i].ID)
		}
	}
}
