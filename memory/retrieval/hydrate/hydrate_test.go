package hydrate

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/component"
	messagesource "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/storage"
	documentview "github.com/GizClaw/flowcraft/memory/views/document"
	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	summaryview "github.com/GizClaw/flowcraft/memory/views/summary"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestCompositeHydratesThreeSourcesAndMissing(t *testing.T) {
	ctx := context.Background()
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
	ws := workspace.NewMemWorkspace()
	messages := newMessageStore(t, ws)
	facts := newFactStore(t, ws)
	chunks := newDocumentView(t, ws)
	records, err := messages.Append(ctx, messagesource.AppendRequest{
		Scope: scope, ConversationID: "conversation", IdempotencyKey: "turn",
		Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, "message text")},
	})
	if err != nil {
		t.Fatal(err)
	}
	source := sdkmemory.SourceRef{
		Kind: sdkmemory.SourceMessage, ID: "conversation/" + records[0].ID, Revision: "1",
	}
	if _, err := facts.Add(ctx, factview.AddRequest{
		ID: "fact-1", Scope: scope, ConversationID: "conversation", Content: textContent("fact text"),
		Provenance: []sdkmemory.SourceRef{source},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := chunks.ReplaceDocument(ctx, documentview.ReplaceRequest{
		Scope: scope, DatasetID: "dataset", DocumentID: "document", DocumentVersion: 1,
		Chunks: []documentview.Chunk{{
			ID: "chunk-1", Scope: scope, DatasetID: "dataset", DocumentID: "document",
			DocumentVersion: 1, Content: textContent("chunk text"), Provenance: []sdkmemory.SourceRef{source},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	hydrator := &Composite{Messages: messages, Facts: facts, Chunks: chunks}
	tests := []struct {
		address component.CandidateAddress
		text    string
	}{
		{component.CandidateAddress{Kind: sdkmemory.ContextRawMessage, ConversationID: "conversation", ItemID: records[0].ID}, "message text"},
		{component.CandidateAddress{Kind: sdkmemory.ContextFact, ConversationID: "conversation", ItemID: "fact-1"}, "fact text"},
		{component.CandidateAddress{Kind: sdkmemory.ContextDocumentChunk, DatasetID: "dataset", DocumentID: "document", ItemID: "chunk-1"}, "chunk text"},
	}
	for _, test := range tests {
		item, err := hydrator.Hydrate(ctx, scope, hydratedCandidate(test.address, source))
		if err != nil || item.Content.Text() != test.text || item.Score != 0.8 || len(item.Sources) == 0 ||
			item.Address.ItemID != test.address.ItemID {
			t.Fatalf("Hydrate(%+v) = %+v, %v", test.address, item, err)
		}
		stale := source
		stale.Revision = "stale"
		if _, err := hydrator.Hydrate(ctx, scope, hydratedCandidate(test.address, stale)); err == nil {
			t.Fatalf("Hydrate(%+v) accepted stale provenance", test.address)
		}
	}
	missing := component.CandidateAddress{Kind: sdkmemory.ContextFact, ConversationID: "conversation", ItemID: "missing"}
	if _, err := hydrator.Hydrate(ctx, scope, hydratedCandidate(missing, source)); err == nil {
		t.Fatal("missing fact did not fail")
	}
	if _, err := chunks.ReplaceDocument(ctx, documentview.ReplaceRequest{
		Scope: scope, DatasetID: "dataset", DocumentID: "document", DocumentVersion: 2,
		Chunks: []documentview.Chunk{},
	}); err != nil {
		t.Fatal(err)
	}
	oldChunk := component.CandidateAddress{
		Kind: sdkmemory.ContextDocumentChunk, DatasetID: "dataset", DocumentID: "document", ItemID: "chunk-1",
	}
	if _, err := hydrator.Hydrate(ctx, scope, hydratedCandidate(oldChunk, source)); err == nil {
		t.Fatal("tombstoned build chunk remained hydratable")
	}
}

func newMessageStore(t *testing.T, ws workspace.Workspace) *messagesource.MessageStore {
	t.Helper()
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := messagesource.NewMessageStore(logStore)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func newDocumentView(t *testing.T, ws workspace.Workspace) *documentview.DocumentViewStore {
	t.Helper()
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := documentview.NewDocumentViewStore(kvStore)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func newFactStore(t *testing.T, ws workspace.Workspace, options ...factview.Option) *factview.FactStore {
	t.Helper()
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := factview.NewFactStore(logStore, kvStore, options...)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func newSummaryStore(t *testing.T, ws workspace.Workspace, options ...summaryview.Option) *summaryview.SummaryStore {
	t.Helper()
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := summaryview.NewSummaryStore(logStore, kvStore, options...)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func hydratedCandidate(address component.CandidateAddress, source sdkmemory.SourceRef) component.Candidate {
	return component.Candidate{
		ID: address.ItemID, Lane: "fusion", Name: string(address.Kind), Score: 0.8, Address: address,
		Source: source,
	}
}

func textContent(text string) sdkmessage.Content {
	return sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: text}}}
}
