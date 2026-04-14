package memory

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func BenchmarkAssemble_100Messages(b *testing.B) {
	ss := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	ms := NewInMemoryStore()
	ml := &mockSummaryLLM{}
	cfg := DefaultDAGConfig()
	cfg.TokenBudget = 2000
	dag := NewSummaryDAG(ss, ms, ml, cfg, &EstimateCounter{})

	ctx := context.Background()
	convID := "bench-assemble-100"

	msgs := make([]model.Message, 100)
	msgs[0] = model.NewTextMessage(model.RoleSystem, "You are a helpful assistant.")
	for i := 1; i < 100; i++ {
		if i%2 == 1 {
			msgs[i] = model.NewTextMessage(model.RoleUser, fmt.Sprintf("User message %d with some content to test token counting", i))
		} else {
			msgs[i] = model.NewTextMessage(model.RoleAssistant, fmt.Sprintf("Assistant response %d with detailed explanation of something", i))
		}
	}
	_ = ms.SaveMessages(ctx, convID, msgs)

	for i := 0; i < 10; i++ {
		ss.data[convID] = append(ss.data[convID], &SummaryNode{
			ID: fmt.Sprintf("leaf-%d", i), ConversationID: convID, Depth: 0,
			Content:     fmt.Sprintf("Summary of messages %d-%d", i*10, i*10+9),
			EarliestSeq: i * 10, LatestSeq: i*10 + 9, TokenCount: 20, CreatedAt: time.Now(),
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = dag.Assemble(ctx, convID, 2000)
	}
}

func BenchmarkAssemble_1000Messages(b *testing.B) {
	ss := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	ms := NewInMemoryStore()
	ml := &mockSummaryLLM{}
	cfg := DefaultDAGConfig()
	cfg.TokenBudget = 4000
	dag := NewSummaryDAG(ss, ms, ml, cfg, &EstimateCounter{})

	ctx := context.Background()
	convID := "bench-assemble-1000"

	msgs := make([]model.Message, 1000)
	msgs[0] = model.NewTextMessage(model.RoleSystem, "System prompt.")
	for i := 1; i < 1000; i++ {
		if i%2 == 1 {
			msgs[i] = model.NewTextMessage(model.RoleUser, fmt.Sprintf("Message %d", i))
		} else {
			msgs[i] = model.NewTextMessage(model.RoleAssistant, fmt.Sprintf("Response %d", i))
		}
	}
	_ = ms.SaveMessages(ctx, convID, msgs)

	for i := 0; i < 100; i++ {
		ss.data[convID] = append(ss.data[convID], &SummaryNode{
			ID: fmt.Sprintf("d0-%d", i), ConversationID: convID, Depth: 0,
			Content:     fmt.Sprintf("Leaf summary %d", i),
			EarliestSeq: i * 10, LatestSeq: i*10 + 9, TokenCount: 15, CreatedAt: time.Now(),
		})
	}
	for i := 0; i < 30; i++ {
		ss.data[convID] = append(ss.data[convID], &SummaryNode{
			ID: fmt.Sprintf("d1-%d", i), ConversationID: convID, Depth: 1,
			Content:     fmt.Sprintf("Condensed summary %d", i),
			EarliestSeq: i * 30, LatestSeq: i*30 + 29, TokenCount: 25, CreatedAt: time.Now(),
		})
	}
	for i := 0; i < 10; i++ {
		ss.data[convID] = append(ss.data[convID], &SummaryNode{
			ID: fmt.Sprintf("d2-%d", i), ConversationID: convID, Depth: 2,
			Content:     fmt.Sprintf("High-level summary %d", i),
			EarliestSeq: i * 100, LatestSeq: i*100 + 99, TokenCount: 30, CreatedAt: time.Now(),
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = dag.Assemble(ctx, convID, 4000)
	}
}

func BenchmarkFileSummaryStore_Search(b *testing.B) {
	ws, err := workspace.NewLocalWorkspace(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	store := NewFileSummaryStore(ws, "mem")
	ctx := context.Background()

	for i := 0; i < 500; i++ {
		_ = store.Save(ctx, &SummaryNode{
			ConversationID: "bench-search",
			Depth:          i % 3,
			Content:        fmt.Sprintf("Summary node %d about topic %d with various keywords", i, i%50),
			EarliestSeq:    i * 10, LatestSeq: i*10 + 9, TokenCount: 20,
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = store.Search(ctx, "bench-search", "topic keywords", 5)
	}
}

func BenchmarkCompact_200Nodes(b *testing.B) {
	ss := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	ms := NewInMemoryStore()
	cfg := DefaultDAGConfig()
	dag := NewSummaryDAG(ss, ms, nil, cfg, &EstimateCounter{})
	ctx := context.Background()
	convID := "bench-compact"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		ss.data[convID] = make([]*SummaryNode, 0, 200)
		for j := 0; j < 200; j++ {
			n := &SummaryNode{
				ID: fmt.Sprintf("n%d", j), ConversationID: convID, Depth: 0,
				Content: fmt.Sprintf("Content for node %d", j), EarliestSeq: j, LatestSeq: j,
				TokenCount: 10, CreatedAt: time.Now(),
			}
			if j < 30 {
				n.Deleted = true
			}
			ss.data[convID] = append(ss.data[convID], n)
		}
		for j := 0; j < 20; j++ {
			ss.data[convID] = append(ss.data[convID], &SummaryNode{
				ID: fmt.Sprintf("d1-%d", j), ConversationID: convID, Depth: 1,
				Content:     fmt.Sprintf("Condensed %d", j),
				SourceIDs:   []string{fmt.Sprintf("n%d", j*3), fmt.Sprintf("n%d", j*3+1), fmt.Sprintf("n%d", j*3+2)},
				EarliestSeq: j * 3, LatestSeq: j*3 + 2, TokenCount: 15, CreatedAt: time.Now(),
			})
		}
		b.StartTimer()
		_, _ = dag.Compact(ctx, convID)
	}
}

func BenchmarkArchive_500Messages(b *testing.B) {
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		ws, _ := workspace.NewLocalWorkspace(b.TempDir())
		store := NewFileStore(ws, "memory")
		convID := "bench-archive"
		msgs := make([]model.Message, 600)
		for j := range msgs {
			msgs[j] = model.NewTextMessage(model.RoleUser, fmt.Sprintf("Message content %d with some text", j))
		}
		_ = store.SaveMessages(ctx, convID, msgs)
		b.StartTimer()

		cfg := ArchiveConfig{ArchiveThreshold: 550, ArchiveBatchSize: 500}
		_, _ = Archive(ctx, ws, store, "memory", convID, cfg)
	}
}

func BenchmarkExpandFromArchive_50Messages(b *testing.B) {
	ws, _ := workspace.NewLocalWorkspace(b.TempDir())
	store := NewFileStore(ws, "memory")
	ctx := context.Background()
	convID := "bench-expand-archive"

	msgs := make([]model.Message, 200)
	for j := range msgs {
		msgs[j] = model.NewTextMessage(model.RoleUser, fmt.Sprintf("Message %d content", j))
	}
	_ = store.SaveMessages(ctx, convID, msgs)

	cfg := ArchiveConfig{ArchiveThreshold: 100, ArchiveBatchSize: 100}
	_, _ = Archive(ctx, ws, store, "memory", convID, cfg)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = LoadArchivedMessages(ctx, ws, "memory", convID, 10, 59)
	}
}
