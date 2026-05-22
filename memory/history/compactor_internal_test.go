package history

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestSummaryDAG_Ingest(t *testing.T) {
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	msgStore := NewInMemoryStore()
	ml := &mockSummaryLLM{}
	cfg := DefaultDAGConfig()
	cfg.ChunkSize = 3
	dag := NewSummaryDAG(summaryStore, msgStore, ml, cfg, &EstimateCounter{})

	ctx := context.Background()
	convID := "dag-test"

	msgs := []model.Message{
		model.NewTextMessage(model.RoleUser, "Hello"),
		model.NewTextMessage(model.RoleAssistant, "Hi"),
		model.NewTextMessage(model.RoleUser, "How are you?"),
		model.NewTextMessage(model.RoleAssistant, "Good"),
		model.NewTextMessage(model.RoleUser, "What's new?"),
		model.NewTextMessage(model.RoleAssistant, "Not much"),
	}

	if err := dag.Ingest(ctx, convID, msgs, 0); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// With chunkSize=3, 6 msgs -> 2 leaf nodes.
	nodes, err := summaryStore.List(ctx, convID, SummaryListOptions{Depth: intPtr(0)})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 leaf nodes, got %d", len(nodes))
	}
}

func TestSummaryDAG_Assemble_SmallConversation(t *testing.T) {
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	msgStore := NewInMemoryStore()
	ml := &mockSummaryLLM{}
	cfg := DefaultDAGConfig()
	cfg.TokenBudget = 10000
	dag := NewSummaryDAG(summaryStore, msgStore, ml, cfg, &EstimateCounter{})

	ctx := context.Background()
	convID := "assemble-test"

	msgs := []model.Message{
		model.NewTextMessage(model.RoleSystem, "You are a helper"),
		model.NewTextMessage(model.RoleUser, "Hello"),
		model.NewTextMessage(model.RoleAssistant, "Hi there"),
	}
	_ = msgStore.SaveMessages(ctx, convID, msgs)

	result, err := dag.Assemble(ctx, convID, 10000)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("small conv should return all msgs, got %d", len(result))
	}
}

func TestSummaryDAG_Compact(t *testing.T) {
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	msgStore := NewInMemoryStore()
	ml := &mockSummaryLLM{}
	cfg := DefaultDAGConfig()
	dag := NewSummaryDAG(summaryStore, msgStore, ml, cfg, &EstimateCounter{})

	ctx := context.Background()
	convID := "compact-test"

	// Add some nodes, mark one deleted.
	summaryStore.data[convID] = []*SummaryNode{
		{ID: "n1", ConversationID: convID, Depth: 0, Content: "first", EarliestSeq: 0, LatestSeq: 9, TokenCount: 10, CreatedAt: time.Now()},
		{ID: "n2", ConversationID: convID, Depth: 0, Content: "second", EarliestSeq: 10, LatestSeq: 19, TokenCount: 10, CreatedAt: time.Now()},
		{ID: "n3", ConversationID: convID, Depth: 1, Content: "condensed", SourceIDs: []string{"n1", "n2"}, EarliestSeq: 0, LatestSeq: 19, TokenCount: 15, CreatedAt: time.Now()},
		{ID: "n4", ConversationID: convID, Deleted: true},
	}

	result, err := dag.Compact(ctx, convID)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if result.DeletedRemoved != 1 {
		t.Fatalf("expected 1 deleted removed, got %d", result.DeletedRemoved)
	}
	// n1 and n2 are d0 with parent -> should be pruned.
	if result.LeafPruned != 2 {
		t.Fatalf("expected 2 leaf pruned, got %d", result.LeafPruned)
	}
	if result.TotalRemaining != 3 {
		t.Fatalf("expected 3 remaining, got %d", result.TotalRemaining)
	}
}

func TestDepthPrompt(t *testing.T) {
	p0 := depthPrompt(0)
	if p0 != leafPrompt {
		t.Fatalf("depth 0 should return leafPrompt")
	}
	p1 := depthPrompt(1)
	if p1 != condensedD1Prompt {
		t.Fatalf("depth 1 should return d1")
	}
	p5 := depthPrompt(5)
	if p5 != condensedD3Prompt {
		t.Fatalf("depth 5 should return d3+")
	}
}

func TestExtractExpandHint(t *testing.T) {
	content := "Some summary.\n[Expand for details about: files, decisions]"
	body, hint, err := extractExpandHint(content)
	if err != nil {
		t.Fatal(err)
	}
	if body != "Some summary." {
		t.Fatalf("unexpected body: %q", body)
	}
	if hint != "[Expand for details about: files, decisions]" {
		t.Fatalf("unexpected hint: %q", hint)
	}
}
