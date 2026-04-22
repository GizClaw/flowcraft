package history

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestBuildSummaryIndex_NilStore(t *testing.T) {
	result := BuildSummaryIndex(context.Background(), nil, "conv1", 1500)
	if result != "" {
		t.Fatalf("expected empty, got %q", result)
	}
}

func TestBuildSummaryIndex_EmptyConvID(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileSummaryStore(ws, "mem")
	result := BuildSummaryIndex(context.Background(), store, "", 1500)
	if result != "" {
		t.Fatalf("expected empty, got %q", result)
	}
}

func TestBuildSummaryIndex_NoSummaries(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileSummaryStore(ws, "mem")
	result := BuildSummaryIndex(context.Background(), store, "conv1", 1500)
	if result != "" {
		t.Fatalf("expected empty, got %q", result)
	}
}

func TestBuildSummaryIndex_SingleNode(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileSummaryStore(ws, "mem")
	ctx := context.Background()

	_ = store.Save(ctx, &SummaryNode{
		ID: "s_abc", ConversationID: "conv1", Depth: 0,
		Content: "用户要求构建 RAG 工作流", EarliestSeq: 0, LatestSeq: 50,
	})

	result := BuildSummaryIndex(ctx, store, "conv1", 1500)

	if !strings.Contains(result, "## Conversation Summary") {
		t.Fatal("missing header")
	}
	if !strings.Contains(result, "[s_abc]") {
		t.Fatal("missing summary ID")
	}
	if !strings.Contains(result, "seq 0-50") {
		t.Fatal("missing seq range")
	}
	if !strings.Contains(result, "memory_expand") {
		t.Fatal("missing memory_expand hint")
	}
}

func TestBuildSummaryIndex_MultipleNodes_TopDepth(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileSummaryStore(ws, "mem")
	ctx := context.Background()

	_ = store.Save(ctx, &SummaryNode{
		ID: "leaf1", ConversationID: "conv1", Depth: 0,
		Content: "leaf summary 1", EarliestSeq: 0, LatestSeq: 10,
	})
	_ = store.Save(ctx, &SummaryNode{
		ID: "leaf2", ConversationID: "conv1", Depth: 0,
		Content: "leaf summary 2", EarliestSeq: 11, LatestSeq: 20,
	})
	_ = store.Save(ctx, &SummaryNode{
		ID: "top1", ConversationID: "conv1", Depth: 1,
		Content: "condensed summary covering leaves", EarliestSeq: 0, LatestSeq: 20,
		SourceIDs: []string{"leaf1", "leaf2"},
	})

	result := BuildSummaryIndex(ctx, store, "conv1", 1500)

	if !strings.Contains(result, "[top1]") {
		t.Fatal("should include top-depth node")
	}
	if strings.Contains(result, "[leaf1]") || strings.Contains(result, "[leaf2]") {
		t.Fatal("should NOT include lower-depth nodes")
	}
}

func TestBuildSummaryIndex_BudgetTruncation(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileSummaryStore(ws, "mem")
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		_ = store.Save(ctx, &SummaryNode{
			ID:             fmt.Sprintf("s_%03d", i),
			ConversationID: "conv1", Depth: 0,
			Content:     strings.Repeat("长文本内容用于测试截断", 10),
			EarliestSeq: i * 10, LatestSeq: i*10 + 9,
		})
	}

	result := BuildSummaryIndex(ctx, store, "conv1", 500)

	if len(result) > 600 {
		t.Fatalf("result too long (%d chars), budget should limit it", len(result))
	}
	if !strings.Contains(result, "omitted") {
		t.Fatal("should contain omission note when budget truncates")
	}
}

func TestBuildSummaryIndex_DeletedNodesExcluded(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileSummaryStore(ws, "mem")
	ctx := context.Background()

	_ = store.Save(ctx, &SummaryNode{
		ID: "alive", ConversationID: "conv1", Depth: 0,
		Content: "alive node", EarliestSeq: 0, LatestSeq: 10,
	})
	_ = store.Save(ctx, &SummaryNode{
		ID: "dead", ConversationID: "conv1", Depth: 0,
		Content: "dead node", EarliestSeq: 11, LatestSeq: 20,
	})
	_ = store.Save(ctx, &SummaryNode{
		ID: "dead", ConversationID: "conv1", Deleted: true,
	})

	result := BuildSummaryIndex(ctx, store, "conv1", 1500)

	if !strings.Contains(result, "[alive]") {
		t.Fatal("should include alive node")
	}
	if strings.Contains(result, "[dead]") {
		t.Fatal("should NOT include deleted node")
	}
}

func TestBuildSummaryIndex_OrderedByEarliestSeq(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileSummaryStore(ws, "mem")
	ctx := context.Background()

	_ = store.Save(ctx, &SummaryNode{
		ID: "later", ConversationID: "conv1", Depth: 0,
		Content: "later", EarliestSeq: 50, LatestSeq: 80,
	})
	_ = store.Save(ctx, &SummaryNode{
		ID: "earlier", ConversationID: "conv1", Depth: 0,
		Content: "earlier", EarliestSeq: 0, LatestSeq: 30,
	})

	result := BuildSummaryIndex(ctx, store, "conv1", 1500)

	earlierIdx := strings.Index(result, "[earlier]")
	laterIdx := strings.Index(result, "[later]")
	if earlierIdx < 0 || laterIdx < 0 {
		t.Fatal("missing node IDs")
	}
	if earlierIdx > laterIdx {
		t.Fatal("earlier should appear before later")
	}
}
