package message

import (
	"bytes"
	"context"
	"encoding/json"
	"path"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

func newTestWorkspace() sdkworkspace.Workspace {
	root := sdkworkspace.NewMemWorkspace()
	return sdkworkspace.Sub(root, "memory/sources/message-test")
}

func TestWorkspaceStore_AppendAssignsAuthoritativeFieldsAndPreservesOrder(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 8, 14, 0, 0, 0, time.UTC)
	explicitCreatedAt := now.Add(time.Hour)
	store := NewWorkspaceStore(newTestWorkspace(), WithClock(func() time.Time { return now }))

	got, err := store.Append(ctx, AppendRequest{
		ConversationID: "conv-1",
		Messages: []Message{
			{
				Message:  textPayload(model.RoleUser, "hello"),
				Metadata: map[string]any{"source": "test"},
			},
			{
				ID:             "provided-id",
				ConversationID: "conv-1",
				Seq:            99,
				Message:        textPayload(model.RoleAssistant, "hi"),
				CreatedAt:      explicitCreatedAt,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Append returned %d messages, want 2", len(got))
	}
	if got[0].ID == "" {
		t.Fatal("first appended message ID is empty")
	}
	if got[0].ConversationID != "conv-1" {
		t.Fatalf("first conversation ID = %q, want conv-1", got[0].ConversationID)
	}
	if got[0].Seq != 1 {
		t.Fatalf("first Seq = %d, want 1", got[0].Seq)
	}
	if got[0].Role != model.RoleUser {
		t.Fatalf("first Role = %q, want user", got[0].Role)
	}
	if got[0].Parts[0].Text != "hello" {
		t.Fatalf("first text part = %q, want hello", got[0].Parts[0].Text)
	}
	if !got[0].CreatedAt.Equal(now) {
		t.Fatalf("first CreatedAt = %v, want %v", got[0].CreatedAt, now)
	}
	if got[1].ID != "provided-id" {
		t.Fatalf("second ID = %q, want provided-id", got[1].ID)
	}
	if got[1].Seq != 2 {
		t.Fatalf("second Seq = %d, want caller-provided Seq overridden to 2", got[1].Seq)
	}
	if !got[1].CreatedAt.Equal(explicitCreatedAt) {
		t.Fatalf("second CreatedAt = %v, want preserved %v", got[1].CreatedAt, explicitCreatedAt)
	}

	listed, err := store.List(ctx, "conv-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("List returned %d messages, want 2", len(listed))
	}
	if listed[0].Parts[0].Text != "hello" || listed[1].Parts[0].Text != "hi" {
		t.Fatalf("List order = [%q, %q], want [hello, hi]", listed[0].Parts[0].Text, listed[1].Parts[0].Text)
	}
}

func TestWorkspaceStore_RecreatedStoreReadsPersistedMessages(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	ws := sdkworkspace.Sub(root, "memory/sources/message-test")
	store := NewWorkspaceStore(ws)
	appended, err := store.Append(ctx, AppendRequest{
		ConversationID: "conv-1",
		Messages: []Message{
			{Message: textPayload(model.RoleUser, "persist me")},
			{Message: textPayload(model.RoleAssistant, "still here")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	recreated := NewWorkspaceStore(ws)
	got, ok, err := recreated.Get(ctx, "conv-1", appended[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get from recreated store ok = false, want true")
	}
	if got.Parts[0].Text != "persist me" {
		t.Fatalf("Get text = %q, want persist me", got.Parts[0].Text)
	}

	listed, err := recreated.List(ctx, "conv-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("List from recreated store returned %d messages, want 2", len(listed))
	}
	if listed[1].ID != appended[1].ID || listed[1].Parts[0].Text != "still here" {
		t.Fatalf("second persisted message = (%q, %q), want (%q, still here)", listed[1].ID, listed[1].Parts[0].Text, appended[1].ID)
	}
}

func TestWorkspaceStore_ModelPayloadRoundTripAndClone(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	ws := sdkworkspace.Sub(root, "memory/sources/message-test")
	store := NewWorkspaceStore(ws)
	payload := model.Message{
		Role: model.RoleAssistant,
		Parts: []model.Part{
			{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "call-1", Name: "lookup", Arguments: `{"query":"flowcraft"}`}},
			{Type: model.PartToolResult, ToolResult: &model.ToolResult{ToolCallID: "call-1", Content: "result", IsError: true}},
			{Type: model.PartData, Data: &model.DataRef{MimeType: "application/json", Value: map[string]any{
				"count":  float64(2),
				"nested": map[string]any{"ok": true},
			}}},
			{Type: model.PartFile, File: &model.FileRef{URI: "workspace://artifact.json", MimeType: "application/json", Name: "artifact.json"}},
			{Type: model.PartImage, Image: &model.MediaRef{URL: "https://example.test/image.png", MediaType: "image/png"}},
		},
	}
	appended, err := store.Append(ctx, AppendRequest{
		ConversationID: "conv-1",
		Messages:       []Message{{Message: payload}},
	})
	if err != nil {
		t.Fatal(err)
	}

	recreated := NewWorkspaceStore(ws)
	got, ok, err := recreated.Get(ctx, "conv-1", appended[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get ok = false, want true")
	}
	assertModelPayload(t, got.Message)

	listed, err := recreated.List(ctx, "conv-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("List returned %d messages, want 1", len(listed))
	}
	assertModelPayload(t, listed[0].Message)

	got.Parts[0].ToolCall.Name = "mutated"
	got.Parts[1].ToolResult.Content = "mutated"
	got.Parts[2].Data.Value["nested"].(map[string]any)["ok"] = false
	got.Parts[3].File.Name = "mutated"
	got.Parts[4].Image.URL = "https://example.test/mutated.png"
	gotAgain, ok, err := recreated.Get(ctx, "conv-1", appended[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("second Get ok = false, want true")
	}
	assertModelPayload(t, gotAgain.Message)

	listed[0].Parts[0].ToolCall.Name = "mutated again"
	listed[0].Parts[1].ToolResult.Content = "mutated again"
	listed[0].Parts[2].Data.Value["nested"].(map[string]any)["ok"] = false
	listed[0].Parts[3].File.Name = "mutated again"
	listed[0].Parts[4].Image.URL = "https://example.test/mutated-again.png"
	listedAgain, err := recreated.List(ctx, "conv-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listedAgain) != 1 {
		t.Fatalf("second List returned %d messages, want 1", len(listedAgain))
	}
	assertModelPayload(t, listedAgain[0].Message)
}

func TestWorkspaceStore_MetadataUsesJSONSemantics(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	ws := sdkworkspace.Sub(root, "memory/sources/message-test")
	store := NewWorkspaceStore(ws)
	appended, err := store.Append(ctx, AppendRequest{
		ConversationID: "conv-1",
		Messages: []Message{{
			Message: textPayload(model.RoleUser, "metadata"),
			Metadata: map[string]any{
				"int":     7,
				"bool":    true,
				"array":   []any{"item", 2, false},
				"object":  map[string]any{"nested_int": 3, "nested_bool": true},
				"nothing": nil,
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	recreated := NewWorkspaceStore(ws)
	got, ok, err := recreated.Get(ctx, "conv-1", appended[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get ok = false, want true")
	}
	assertMetadataJSONSemantics(t, got.Metadata)

	listed, err := recreated.List(ctx, "conv-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("List returned %d messages, want 1", len(listed))
	}
	assertMetadataJSONSemantics(t, listed[0].Metadata)
}

func TestWorkspaceStore_ConversationIDsUseSafePathSegments(t *testing.T) {
	ctx := context.Background()
	ids := []string{".", "..", "conv/with/slash", "name%percent", "space name"}

	for _, conversationID := range ids {
		t.Run(conversationID, func(t *testing.T) {
			ws := newTestWorkspace()
			store := NewWorkspaceStore(ws)

			appended, err := store.Append(ctx, AppendRequest{
				ConversationID: conversationID,
				Messages:       []Message{{Message: textPayload(model.RoleUser, "path-safe")}},
			})
			if err != nil {
				t.Fatal(err)
			}
			other, err := store.Append(ctx, AppendRequest{
				ConversationID: "other-conversation",
				Messages:       []Message{{Message: textPayload(model.RoleAssistant, "keep me")}},
			})
			if err != nil {
				t.Fatal(err)
			}

			segment := pathSegment(conversationID)
			assertSafeWorkspaceSegment(t, segment, conversationID)
			encodedPath := "conversations/" + segment + "/messages.jsonl"
			if exists, err := ws.Exists(ctx, encodedPath); err != nil || !exists {
				t.Fatalf("encoded messages file exists = %v err %v, want true nil", exists, err)
			}
			rawPath := path.Join("conversations", conversationID, "messages.jsonl")
			if rawPath != encodedPath {
				if exists, err := ws.Exists(ctx, rawPath); err != nil || exists {
					t.Fatalf("raw messages path %q exists = %v err %v, want false nil", rawPath, exists, err)
				}
			}

			got, ok, err := store.Get(ctx, conversationID, appended[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("Get with encoded conversation id ok = false, want true")
			}
			if got.ConversationID != conversationID || got.Parts[0].Text != "path-safe" {
				t.Fatalf("Get = (%q, %q), want original conversation id and text", got.ConversationID, got.Parts[0].Text)
			}

			listed, err := store.List(ctx, conversationID, ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(listed) != 1 || listed[0].ConversationID != conversationID || listed[0].ID != appended[0].ID {
				t.Fatalf("List = %+v, want one message with original conversation id %q", listed, conversationID)
			}

			if err := store.DeleteConversation(ctx, conversationID); err != nil {
				t.Fatal(err)
			}
			if listed, err := store.List(ctx, conversationID, ListOptions{}); err != nil || len(listed) != 0 {
				t.Fatalf("List deleted conversation returned %d messages err %v, want 0 nil", len(listed), err)
			}
			if _, ok, err := store.Get(ctx, conversationID, appended[0].ID); err != nil || ok {
				t.Fatalf("Get deleted conversation = ok %v err %v, want ok false nil err", ok, err)
			}
			kept, ok, err := store.Get(ctx, "other-conversation", other[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatalf("other conversation after DeleteConversation(%q) ok = false, want true", conversationID)
			}
			if kept.Parts[0].Text != "keep me" {
				t.Fatalf("other conversation text after DeleteConversation(%q) = %q, want keep me", conversationID, kept.Parts[0].Text)
			}
		})
	}
}

func TestWorkspaceStore_ListConversationsReturnsDecodedSortedIDs(t *testing.T) {
	ctx := context.Background()
	store := NewWorkspaceStore(newTestWorkspace())

	empty, err := store.ListConversations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty ListConversations returned %v, want empty", empty)
	}

	for _, conversationID := range []string{"zeta", "conv/with/slash", "alpha"} {
		if _, err := store.Append(ctx, AppendRequest{
			ConversationID: conversationID,
			Messages:       []Message{{Message: textPayload(model.RoleUser, conversationID)}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.ListConversations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "conv/with/slash", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListConversations = %v, want %v", got, want)
	}

	if err := store.DeleteConversation(ctx, "conv/with/slash"); err != nil {
		t.Fatal(err)
	}
	got, err = store.ListConversations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"alpha", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListConversations after delete = %v, want %v", got, want)
	}
}

func TestWorkspaceStore_AppendValidatesConversationIDs(t *testing.T) {
	ctx := context.Background()
	store := NewWorkspaceStore(newTestWorkspace())

	if _, err := store.Append(ctx, AppendRequest{}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("empty conversation Append err = %v, want validation", err)
	}

	_, err := store.Append(ctx, AppendRequest{
		ConversationID: "conv-1",
		Messages: []Message{
			{ConversationID: "other-conv", Message: textPayload(model.RoleUser, "wrong conversation")},
		},
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("conflicting conversation Append err = %v, want validation", err)
	}
}

func TestWorkspaceStore_ListAfterSeqAndLimit(t *testing.T) {
	ctx := context.Background()
	store := NewWorkspaceStore(newTestWorkspace())

	_, err := store.Append(ctx, AppendRequest{
		ConversationID: "conv-1",
		Messages: []Message{
			{Message: textPayload(model.RoleUser, "one")},
			{Message: textPayload(model.RoleUser, "two")},
			{Message: textPayload(model.RoleUser, "three")},
			{Message: textPayload(model.RoleUser, "four")},
			{Message: textPayload(model.RoleUser, "five")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.List(ctx, "conv-1", ListOptions{AfterSeq: 2, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d messages, want 2", len(got))
	}
	if got[0].Seq != 3 || got[0].Parts[0].Text != "three" {
		t.Fatalf("first listed message = (%d, %q), want (3, three)", got[0].Seq, got[0].Parts[0].Text)
	}
	if got[1].Seq != 4 || got[1].Parts[0].Text != "four" {
		t.Fatalf("second listed message = (%d, %q), want (4, four)", got[1].Seq, got[1].Parts[0].Text)
	}

	unlimited, err := store.List(ctx, "conv-1", ListOptions{AfterSeq: 3, Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(unlimited) != 2 {
		t.Fatalf("unlimited List returned %d messages, want 2", len(unlimited))
	}
}

func TestWorkspaceStore_MalformedJSONLFailsFast(t *testing.T) {
	ctx := context.Background()
	ws := newTestWorkspace()
	store := NewWorkspaceStore(ws)
	conversationID := "conv-bad-jsonl"
	good := Message{
		ID:             "msg-1",
		ConversationID: conversationID,
		Seq:            1,
		Message:        textPayload(model.RoleUser, "valid"),
		CreatedAt:      time.Date(2026, 6, 8, 15, 0, 0, 0, time.UTC),
	}
	original := append(jsonLine(t, good), []byte(`{"ID":"partial"`)...)
	messagesPath := path.Join("conversations", pathSegment(conversationID), "messages.jsonl")
	if err := ws.Write(ctx, messagesPath, original); err != nil {
		t.Fatal(err)
	}

	if _, err := store.List(ctx, conversationID, ListOptions{}); err == nil {
		t.Fatal("List malformed JSONL err = nil, want error")
	}
	if _, _, err := store.Get(ctx, conversationID, "missing"); err == nil {
		t.Fatal("Get malformed JSONL err = nil, want error")
	}
	if _, err := store.Append(ctx, AppendRequest{
		ConversationID: conversationID,
		Messages:       []Message{{Message: textPayload(model.RoleAssistant, "must not append")}},
	}); err == nil {
		t.Fatal("Append malformed JSONL err = nil, want error")
	}

	after, err := ws.Read(ctx, messagesPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("Append changed malformed JSONL file; before %q after %q", string(original), string(after))
	}
}

func TestWorkspaceStore_GetAndListReturnClones(t *testing.T) {
	ctx := context.Background()
	store := NewWorkspaceStore(newTestWorkspace())
	appended, err := store.Append(ctx, AppendRequest{
		ConversationID: "conv-1",
		Messages: []Message{
			{
				Message:  textPayload(model.RoleUser, "clone me"),
				Metadata: map[string]any{"key": "original"},
				SpanRefs: []SpanRef{{Start: 0, End: 5}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, ok, err := store.Get(ctx, "conv-1", appended[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get ok = false, want true")
	}
	got.Metadata["key"] = "mutated"
	got.SpanRefs[0].End = 99
	got.Parts[0].Text = "mutated"

	gotAgain, ok, err := store.Get(ctx, "conv-1", appended[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("second Get ok = false, want true")
	}
	assertUnmutatedClone(t, gotAgain)

	listed, err := store.List(ctx, "conv-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("List returned %d messages, want 1", len(listed))
	}
	listed[0].Metadata["key"] = "mutated again"
	listed[0].SpanRefs[0].End = 123
	listed[0].Parts[0].Text = "mutated again"

	listedAgain, err := store.List(ctx, "conv-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listedAgain) != 1 {
		t.Fatalf("second List returned %d messages, want 1", len(listedAgain))
	}
	assertUnmutatedClone(t, listedAgain[0])
}

func TestWorkspaceStore_DeleteConversationIsIdempotentAndRemovesWorkspaceData(t *testing.T) {
	ctx := context.Background()
	root := sdkworkspace.NewMemWorkspace()
	ws := sdkworkspace.Sub(root, "memory/sources/message-test")
	store := NewWorkspaceStore(ws)
	appended, err := store.Append(ctx, AppendRequest{
		ConversationID: "conv-1",
		Messages: []Message{
			{Message: textPayload(model.RoleUser, "one")},
			{Message: textPayload(model.RoleUser, "two")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	encodedConversationPath := "conversations/" + pathSegment("conv-1") + "/messages.jsonl"
	if exists, err := ws.Exists(ctx, encodedConversationPath); err != nil || !exists {
		t.Fatalf("messages file exists before delete = %v err %v, want true nil", exists, err)
	}

	if err := store.DeleteConversation(ctx, "conv-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteConversation(ctx, "conv-1"); err != nil {
		t.Fatal(err)
	}

	if exists, err := ws.Exists(ctx, "conversations/"+pathSegment("conv-1")); err != nil || exists {
		t.Fatalf("conversation dir exists after delete = %v err %v, want false nil", exists, err)
	}
	listed, err := store.List(ctx, "conv-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("List after delete returned %d messages, want 0", len(listed))
	}
	if _, ok, err := store.Get(ctx, "conv-1", appended[0].ID); err != nil || ok {
		t.Fatalf("Get first message after delete = ok %v err %v, want ok false nil err", ok, err)
	}
	if _, ok, err := store.Get(ctx, "conv-1", appended[1].ID); err != nil || ok {
		t.Fatalf("Get second message after delete = ok %v err %v, want ok false nil err", ok, err)
	}
}

func TestWorkspaceStore_ConcurrentAppendsAssignUniqueIDsAndSeqs(t *testing.T) {
	ctx := context.Background()
	store := NewWorkspaceStore(newTestWorkspace())
	const count = 64

	var wg sync.WaitGroup
	errs := make(chan error, count)
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Append(ctx, AppendRequest{
				ConversationID: "conv-1",
				Messages:       []Message{{Message: textPayload(model.RoleUser, "message")}},
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	listed, err := store.List(ctx, "conv-1", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != count {
		t.Fatalf("List returned %d messages, want %d", len(listed), count)
	}

	seenIDs := make(map[string]struct{}, count)
	seenSeqs := make(map[uint64]struct{}, count)
	for i, msg := range listed {
		wantSeq := uint64(i + 1)
		if msg.Seq != wantSeq {
			t.Fatalf("message %d Seq = %d, want %d", i, msg.Seq, wantSeq)
		}
		if msg.ID == "" {
			t.Fatalf("message %d ID is empty", i)
		}
		if _, dup := seenIDs[msg.ID]; dup {
			t.Fatalf("duplicate ID %q", msg.ID)
		}
		seenIDs[msg.ID] = struct{}{}
		if _, dup := seenSeqs[msg.Seq]; dup {
			t.Fatalf("duplicate Seq %d", msg.Seq)
		}
		seenSeqs[msg.Seq] = struct{}{}
	}
}

func assertUnmutatedClone(t *testing.T, msg Message) {
	t.Helper()
	if msg.Metadata["key"] != "original" {
		t.Fatalf("stored metadata mutated to %q, want original", msg.Metadata["key"])
	}
	if msg.SpanRefs[0].End != 5 {
		t.Fatalf("stored span ref end mutated to %d, want 5", msg.SpanRefs[0].End)
	}
	if msg.Parts[0].Text != "clone me" {
		t.Fatalf("stored text part mutated to %q, want clone me", msg.Parts[0].Text)
	}
}

func assertModelPayload(t *testing.T, msg model.Message) {
	t.Helper()
	if msg.Role != model.RoleAssistant {
		t.Fatalf("Role = %q, want assistant", msg.Role)
	}
	if len(msg.Parts) != 5 {
		t.Fatalf("Parts len = %d, want 5", len(msg.Parts))
	}
	if msg.Parts[0].ToolCall == nil || msg.Parts[0].ToolCall.Name != "lookup" || msg.Parts[0].ToolCall.Arguments != `{"query":"flowcraft"}` {
		t.Fatalf("ToolCall part = %+v, want lookup call", msg.Parts[0].ToolCall)
	}
	if msg.Parts[1].ToolResult == nil || msg.Parts[1].ToolResult.Content != "result" || !msg.Parts[1].ToolResult.IsError {
		t.Fatalf("ToolResult part = %+v, want error result", msg.Parts[1].ToolResult)
	}
	if msg.Parts[2].Data == nil || msg.Parts[2].Data.MimeType != "application/json" {
		t.Fatalf("Data part = %+v, want application/json data", msg.Parts[2].Data)
	}
	wantData := map[string]any{"count": float64(2), "nested": map[string]any{"ok": true}}
	if !reflect.DeepEqual(msg.Parts[2].Data.Value, wantData) {
		t.Fatalf("Data value = %#v, want %#v", msg.Parts[2].Data.Value, wantData)
	}
	if msg.Parts[3].File == nil || msg.Parts[3].File.URI != "workspace://artifact.json" || msg.Parts[3].File.Name != "artifact.json" {
		t.Fatalf("File part = %+v, want artifact file", msg.Parts[3].File)
	}
	if msg.Parts[4].Image == nil || msg.Parts[4].Image.URL != "https://example.test/image.png" || msg.Parts[4].Image.MediaType != "image/png" {
		t.Fatalf("Image part = %+v, want image ref", msg.Parts[4].Image)
	}
}

func assertMetadataJSONSemantics(t *testing.T, metadata map[string]any) {
	t.Helper()
	if got, ok := metadata["int"].(float64); !ok || got != 7 {
		t.Fatalf("metadata int = %#v (%T), want float64(7)", metadata["int"], metadata["int"])
	}
	if got, ok := metadata["bool"].(bool); !ok || !got {
		t.Fatalf("metadata bool = %#v (%T), want true bool", metadata["bool"], metadata["bool"])
	}
	wantArray := []any{"item", float64(2), false}
	if !reflect.DeepEqual(metadata["array"], wantArray) {
		t.Fatalf("metadata array = %#v, want %#v", metadata["array"], wantArray)
	}
	wantObject := map[string]any{"nested_int": float64(3), "nested_bool": true}
	if !reflect.DeepEqual(metadata["object"], wantObject) {
		t.Fatalf("metadata object = %#v, want %#v", metadata["object"], wantObject)
	}
	if _, ok := metadata["nothing"]; !ok || metadata["nothing"] != nil {
		t.Fatalf("metadata nothing = %#v, want present nil", metadata["nothing"])
	}
}

func assertSafeWorkspaceSegment(t *testing.T, segment, raw string) {
	t.Helper()
	if segment == "" {
		t.Fatal("encoded path segment is empty")
	}
	if segment == "." || segment == ".." {
		t.Fatalf("encoded path segment = %q, want non-special segment", segment)
	}
	if strings.Contains(segment, "/") {
		t.Fatalf("encoded path segment %q contains slash", segment)
	}
	if segment == raw {
		t.Fatalf("encoded path segment = raw id %q", raw)
	}
}

func jsonLine(t *testing.T, msg Message) []byte {
	t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func textPayload(role model.Role, text string) model.Message {
	return model.Message{
		Role: role,
		Parts: []model.Part{
			{Type: model.PartText, Text: text},
		},
	}
}
