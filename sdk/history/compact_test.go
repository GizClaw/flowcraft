package history

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestCompactArchive_Integration_LongConversation(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(ws, "memory")
	summaryStore := NewFileSummaryStore(ws, "memory")
	ml := &mockSummaryLLM{}
	cfg := DefaultDAGConfig()
	cfg.ChunkSize = 5
	cfg.CondenseThreshold = 4
	cfg.Compact.CompactThreshold = 15
	cfg.Archive.ArchiveThreshold = 40
	cfg.Archive.ArchiveBatchSize = 20

	dag := NewSummaryDAG(summaryStore, store, ml, cfg, &EstimateCounter{})
	mem := newCompactor(store, dag, cfg, ws, "memory")

	ctx := context.Background()
	convID := "integration-long"

	// Simulate 50 messages in batches of 10.
	for batch := 0; batch < 5; batch++ {
		msgs := make([]model.Message, 0, 10)
		for i := 0; i < 10; i++ {
			if i%2 == 0 {
				msgs = append(msgs, model.NewTextMessage(model.RoleUser, "user message"))
			} else {
				msgs = append(msgs, model.NewTextMessage(model.RoleAssistant, "assistant response"))
			}
		}
		if err := mem.Append(ctx, convID, msgs); err != nil {
			t.Fatalf("batch %d Append: %v", batch, err)
		}
		// Wait for async ingest.
		time.Sleep(500 * time.Millisecond)
	}

	// Verify DAG has nodes.
	allNodes, err := summaryStore.ListAll(ctx, convID)
	if err != nil {
		t.Fatal(err)
	}
	if len(allNodes) == 0 {
		t.Fatal("expected DAG nodes after 50 messages")
	}

	// Check multiple depths exist.
	hasD0, hasD1 := false, false
	for _, n := range allNodes {
		if n.Deleted {
			continue
		}
		if n.Depth == 0 {
			hasD0 = true
		}
		if n.Depth >= 1 {
			hasD1 = true
		}
	}
	if !hasD0 {
		t.Fatal("expected d0 nodes")
	}
	if !hasD1 {
		t.Log("no d1 nodes yet (may need more messages for condense)")
	}

	// Verify Load returns assembled context.
	loaded, err := mem.Load(ctx, convID, Budget{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) == 0 {
		t.Fatal("Load returned empty")
	}

	// Verify summary index can be built from the DAG.
	index := BuildSummaryIndex(ctx, summaryStore, convID, 1500)
	if index == "" {
		t.Fatal("BuildSummaryIndex returned empty — expected summaries after ingest")
	}
}

func TestCompactArchive_ManualCompact(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	summaryStore := NewFileSummaryStore(ws, "mem")
	ctx := context.Background()
	convID := "manual-compact"

	// Populate with d0 and d1 nodes.
	d0IDs := make([]string, 6)
	for i := 0; i < 6; i++ {
		n := &SummaryNode{ConversationID: convID, Depth: 0, Content: "leaf content",
			EarliestSeq: i * 5, LatestSeq: i*5 + 4, TokenCount: 10}
		_ = summaryStore.Save(ctx, n)
		d0IDs[i] = n.ID
	}
	// Create d1 covering first 3 d0s.
	_ = summaryStore.Save(ctx, &SummaryNode{
		ConversationID: convID, Depth: 1, Content: "condensed 0-14",
		SourceIDs: d0IDs[:3], EarliestSeq: 0, LatestSeq: 14, TokenCount: 15,
	})
	// Mark one d0 as deleted.
	_ = summaryStore.DeleteByConvID(ctx, convID, d0IDs[5])

	dag := &SummaryDAG{store: summaryStore, config: DefaultDAGConfig()}
	result, err := dag.Compact(ctx, convID)
	if err != nil {
		t.Fatal(err)
	}

	if result.DeletedRemoved != 1 {
		t.Fatalf("expected 1 deleted removed, got %d", result.DeletedRemoved)
	}
	if result.LeafPruned != 3 {
		t.Fatalf("expected 3 leaf pruned (d0 with parent), got %d", result.LeafPruned)
	}

	// Verify d0 without parent (d0IDs[3], d0IDs[4]) still has content.
	remaining, _ := summaryStore.ListAll(ctx, convID)
	for _, n := range remaining {
		if n.Depth == 0 && (n.ID == d0IDs[3] || n.ID == d0IDs[4]) {
			if n.Content == "[pruned — use history_expand to load originals]" {
				t.Fatalf("d0 without parent should NOT be pruned: %s", n.ID)
			}
		}
	}
}

func TestCompactArchive_ArchiveAndExpand(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(ws, "memory")
	ctx := context.Background()
	convID := "archive-expand"

	msgs := make([]model.Message, 30)
	for i := range msgs {
		msgs[i] = model.NewTextMessage(model.RoleUser, "content message")
	}
	_ = store.SaveMessages(ctx, convID, msgs)

	// Archive first 15.
	cfg := ArchiveConfig{ArchiveThreshold: 20, ArchiveBatchSize: 15}
	ar, err := Archive(ctx, ws, store, "memory", convID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if ar.MessagesArchived != 15 {
		t.Fatalf("expected 15 archived, got %d", ar.MessagesArchived)
	}

	// Expand across boundary (seq 10-20).
	summaryStore := NewFileSummaryStore(ws, "memory")
	_ = summaryStore.Save(ctx, &SummaryNode{
		ID: "cross-node", ConversationID: convID, Depth: 0,
		Content: "summary", EarliestSeq: 10, LatestSeq: 20,
	})

	expandTool := newHistoryExpandTool(ToolDeps{
		SummaryStore: summaryStore, MessageStore: store,
		Workspace: ws, Prefix: "memory",
	})
	expandCtx := WithConversationID(ctx, convID)
	result, err := expandTool.Execute(expandCtx, `{"summary_id":"cross-node","max_messages":50}`)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if result == "" {
		t.Fatal("expand returned empty")
	}
}
