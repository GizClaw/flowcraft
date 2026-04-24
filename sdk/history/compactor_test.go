package history

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// mockLLM implements llm.LLM for testing.
type mockSummaryLLM struct {
	response string
}

func (m *mockSummaryLLM) Generate(_ context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	resp := m.response
	if resp == "" {
		resp = "Summary of the conversation.\n[Expand for details about: test topics]"
	}
	return llm.NewTextMessage(llm.RoleAssistant, resp), llm.TokenUsage{}, nil
}

func (m *mockSummaryLLM) GenerateStream(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, nil
}

func TestCompacted_SaveAndLoad(t *testing.T) {
	store := NewInMemoryStore()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	ml := &mockSummaryLLM{}
	dag := NewSummaryDAG(summaryStore, store, ml, DefaultDAGConfig(), &EstimateCounter{})
	mem := newCompactor(store, dag, DefaultDAGConfig(), nil, "")

	ctx := context.Background()
	convID := "test-conv-1"

	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, "You are a helper"),
		llm.NewTextMessage(llm.RoleUser, "Hello"),
		llm.NewTextMessage(llm.RoleAssistant, "Hi there"),
	}

	if err := mem.Append(ctx, convID, msgs); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Wait for async ingest.
	time.Sleep(200 * time.Millisecond)

	loaded, err := mem.Load(ctx, convID, Budget{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(loaded) == 0 {
		t.Fatal("Load returned empty messages")
	}

	// Should have system + user + assistant (3 msgs fits in budget).
	if len(loaded) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(loaded))
	}
}

func TestCompacted_Clear(t *testing.T) {
	store := NewInMemoryStore()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	ml := &mockSummaryLLM{}
	dag := NewSummaryDAG(summaryStore, store, ml, DefaultDAGConfig(), &EstimateCounter{})
	mem := newCompactor(store, dag, DefaultDAGConfig(), nil, "")

	ctx := context.Background()
	convID := "test-clear"

	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "Hello"),
	}
	_ = mem.Append(ctx, convID, msgs)

	if err := mem.Clear(ctx, convID); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	loaded, err := mem.Load(ctx, convID, Budget{})
	if err != nil {
		t.Fatalf("Load after clear: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("expected 0 after clear, got %d", len(loaded))
	}
}

// inMemSummaryStore is an in-memory SummaryStore for testing.
type inMemSummaryStore struct {
	mu   sync.Mutex
	data map[string][]*SummaryNode
}

func (s *inMemSummaryStore) Save(_ context.Context, node *SummaryNode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if node.ID == "" {
		node.ID = NewSummaryNodeID()
	}
	if node.CreatedAt.IsZero() {
		node.CreatedAt = time.Now()
	}
	s.data[node.ConversationID] = append(s.data[node.ConversationID], node)
	return nil
}

func (s *inMemSummaryStore) GetByConvID(_ context.Context, convID, id string) (*SummaryNode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.data[convID] {
		if n.ID == id && !n.Deleted {
			return n, nil
		}
	}
	return nil, fmt.Errorf("node %q not found in conversation %q", id, convID)
}

func (s *inMemSummaryStore) List(_ context.Context, convID string, opts SummaryListOptions) ([]*SummaryNode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []*SummaryNode
	deleted := make(map[string]bool)
	for _, n := range s.data[convID] {
		if n.Deleted {
			deleted[n.ID] = true
		}
	}
	for _, n := range s.data[convID] {
		if deleted[n.ID] || n.Deleted {
			continue
		}
		if opts.Depth != nil && n.Depth != *opts.Depth {
			continue
		}
		result = append(result, n)
	}
	return result, nil
}

func (s *inMemSummaryStore) Search(_ context.Context, _ string, _ string, _ int) ([]*SummaryNode, error) {
	return nil, nil
}

func (s *inMemSummaryStore) DeleteByConvID(_ context.Context, convID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, n := range s.data[convID] {
		if n.ID == id {
			s.data[convID][i].Deleted = true
			return nil
		}
	}
	return nil
}

func (s *inMemSummaryStore) ListAll(_ context.Context, convID string) ([]*SummaryNode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[convID], nil
}

func (s *inMemSummaryStore) Rewrite(_ context.Context, convID string, nodes []*SummaryNode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[convID] = nodes
	return nil
}

func TestCompacted_CloseWaitsForAsync(t *testing.T) {
	store := NewInMemoryStore()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	ml := &mockSummaryLLM{}
	dag := NewSummaryDAG(summaryStore, store, ml, DefaultDAGConfig(), &EstimateCounter{})
	mem := newCompactor(store, dag, DefaultDAGConfig(), nil, "")

	ctx := context.Background()

	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "Hello"),
		llm.NewTextMessage(llm.RoleAssistant, "Hi"),
	}

	for i := 0; i < 5; i++ {
		if err := mem.Append(ctx, fmt.Sprintf("conv-%d", i), msgs); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}

	// Close should block until all async goroutines complete (not panic or deadlock).
	done := make(chan struct{})
	go func() {
		mem.Close()
		close(done)
	}()

	select {
	case <-done:
		// Success — Close returned after all async work finished.
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return within 10 seconds; possible deadlock")
	}

	// After Close, all conversations should have been ingested.
	for i := 0; i < 5; i++ {
		loaded, err := store.GetMessages(ctx, fmt.Sprintf("conv-%d", i))
		if err != nil {
			t.Fatalf("GetMessages conv-%d: %v", i, err)
		}
		if len(loaded) != 2 {
			t.Fatalf("conv-%d: expected 2 messages, got %d", i, len(loaded))
		}
	}
}

// TestCompacted_NoIngestDrop pins down the post-refactor invariant:
// fast successive Appends across many conversations must NOT silently drop
// any DAG ingest. The old semaphore-bounded implementation could skip
// ingests under load (telemetry warned, but the summarized history quietly
// shrank); the new per-conversation goroutine model has no such ceiling.
func TestCompacted_NoIngestDrop(t *testing.T) {
	store := NewInMemoryStore()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	slowLLM := &slowMockLLM{delay: 50 * time.Millisecond}
	dag := NewSummaryDAG(summaryStore, store, slowLLM, DefaultDAGConfig(), &EstimateCounter{})
	mem := newCompactor(store, dag, DefaultDAGConfig(), nil, "")

	ctx := context.Background()
	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "Hello"),
		llm.NewTextMessage(llm.RoleAssistant, "Hi"),
	}

	const conversations = 20
	for i := 0; i < conversations; i++ {
		if err := mem.Append(ctx, fmt.Sprintf("conv-%d", i), msgs); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	mem.Close()

	for i := 0; i < conversations; i++ {
		got, err := store.GetMessages(ctx, fmt.Sprintf("conv-%d", i))
		if err != nil {
			t.Fatalf("GetMessages conv-%d: %v", i, err)
		}
		if len(got) != 2 {
			t.Fatalf("conv-%d: expected 2 messages, got %d", i, len(got))
		}
	}
}

type slowMockLLM struct {
	delay time.Duration
}

func (m *slowMockLLM) Generate(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	time.Sleep(m.delay)
	return llm.NewTextMessage(llm.RoleAssistant, "Summary.\n[Expand for details]"), llm.TokenUsage{}, nil
}

func (m *slowMockLLM) GenerateStream(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, nil
}

// --- CompactOption helpers (functional options on compactOptions) ---

func TestCompactOptions_AllSetters(t *testing.T) {
	o := compactOptions{dag: DefaultDAGConfig(), counter: &EstimateCounter{}, prefix: "memory"}

	custom := DefaultDAGConfig()
	custom.ChunkSize = 999
	WithDAGConfig(custom)(&o)
	if o.dag.ChunkSize != 999 {
		t.Fatalf("WithDAGConfig: ChunkSize=%d", o.dag.ChunkSize)
	}

	WithChunkSize(7)(&o)
	WithCondenseThreshold(8)(&o)
	WithMaxDepth(9)(&o)
	WithTokenBudget(1234)(&o)
	WithRecentRatio(0.42)(&o)
	WithCompactThreshold(55)(&o)
	WithLeafPrune(false)(&o)
	WithArchiveThreshold(77)(&o)
	WithArchiveBatchSize(33)(&o)
	WithStoragePrefix("custom-prefix")(&o)

	tk, err := NewTiktokenCounter("gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	WithTokenCounter(tk)(&o)

	if o.dag.ChunkSize != 7 {
		t.Fatalf("ChunkSize = %d", o.dag.ChunkSize)
	}
	if o.dag.CondenseThreshold != 8 {
		t.Fatalf("CondenseThreshold = %d", o.dag.CondenseThreshold)
	}
	if o.dag.MaxDepth != 9 {
		t.Fatalf("MaxDepth = %d", o.dag.MaxDepth)
	}
	if o.dag.TokenBudget != 1234 {
		t.Fatalf("TokenBudget = %d", o.dag.TokenBudget)
	}
	if o.dag.RecentRatio != 0.42 {
		t.Fatalf("RecentRatio = %v", o.dag.RecentRatio)
	}
	if o.dag.Compact.CompactThreshold != 55 {
		t.Fatalf("CompactThreshold = %d", o.dag.Compact.CompactThreshold)
	}
	if o.dag.Compact.PruneLeafContent != false {
		t.Fatal("PruneLeafContent expected false")
	}
	if o.dag.Archive.ArchiveThreshold != 77 {
		t.Fatalf("ArchiveThreshold = %d", o.dag.Archive.ArchiveThreshold)
	}
	if o.dag.Archive.ArchiveBatchSize != 33 {
		t.Fatalf("ArchiveBatchSize = %d", o.dag.Archive.ArchiveBatchSize)
	}
	if o.prefix != "custom-prefix" {
		t.Fatalf("prefix = %q", o.prefix)
	}
	if o.counter != tk {
		t.Fatal("WithTokenCounter did not swap counter")
	}
}

func TestCompactOptions_RejectNonPositive(t *testing.T) {
	o := compactOptions{dag: DefaultDAGConfig(), counter: &EstimateCounter{}, prefix: "memory"}
	original := o
	// All numeric setters guard against <=0; calling them with bad values
	// must be a no-op rather than corrupting the config.
	WithChunkSize(0)(&o)
	WithCondenseThreshold(-1)(&o)
	WithMaxDepth(0)(&o)
	WithTokenBudget(-1)(&o)
	WithRecentRatio(0)(&o)
	WithCompactThreshold(0)(&o)
	WithArchiveThreshold(-2)(&o)
	WithArchiveBatchSize(0)(&o)
	WithTokenCounter(nil)(&o)
	WithStoragePrefix("")(&o)

	if o.dag != original.dag {
		t.Fatal("zero/negative inputs corrupted DAGConfig")
	}
	if o.counter != original.counter {
		t.Fatal("nil token counter overwrote default")
	}
	if o.prefix != original.prefix {
		t.Fatalf("empty prefix overwrote default, got %q", o.prefix)
	}
}

// --- SummaryDAG.countMsg covers every Part variant ---

func TestSummaryDAG_CountMsg_AllPartTypes(t *testing.T) {
	dag := NewSummaryDAG(
		&inMemSummaryStore{data: make(map[string][]*SummaryNode)},
		NewInMemoryStore(),
		&mockSummaryLLM{},
		DefaultDAGConfig(),
		&EstimateCounter{},
	)

	// Empty message: just the +4 base overhead.
	empty := llm.Message{Role: llm.RoleUser}
	if got := dag.countMsg(empty); got != 4 {
		t.Fatalf("expected base overhead 4 for empty msg, got %d", got)
	}

	full := llm.Message{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "hello world"},
			{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{Name: "tool", Arguments: `{"k":"v"}`}},
			{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{Content: "result body"}},
		},
	}
	got := dag.countMsg(full)
	if got <= 4 {
		t.Fatalf("expected count > base overhead for full msg, got %d", got)
	}

	// Nil ToolCall / ToolResult parts must not panic and must be ignored.
	withNils := llm.Message{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{
			{Type: llm.PartToolCall},
			{Type: llm.PartToolResult},
		},
	}
	if got := dag.countMsg(withNils); got != 4 {
		t.Fatalf("expected base overhead 4 for nil-payload parts, got %d", got)
	}
}

// --- SummaryDAG.Assemble: trim + summary path ---

func TestSummaryDAG_Assemble_NoMessages(t *testing.T) {
	store := NewInMemoryStore()
	defer store.Close()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	dag := NewSummaryDAG(summaryStore, store, &mockSummaryLLM{}, DefaultDAGConfig(), &EstimateCounter{})

	got, err := dag.Assemble(context.Background(), "empty-conv", 1000)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for empty conv, got %d msgs", len(got))
	}
}

func TestSummaryDAG_Assemble_BelowBudgetReturnsAll(t *testing.T) {
	store := NewInMemoryStore()
	defer store.Close()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	dag := NewSummaryDAG(summaryStore, store, &mockSummaryLLM{}, DefaultDAGConfig(), &EstimateCounter{})

	ctx := context.Background()
	convID := "small"
	msgs := []model.Message{
		model.NewTextMessage(model.RoleUser, "hi"),
		model.NewTextMessage(model.RoleAssistant, "hello"),
	}
	_ = store.SaveMessages(ctx, convID, msgs)

	got, err := dag.Assemble(ctx, convID, 1000)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected all 2 msgs back when below budget, got %d", len(got))
	}
}

func TestSummaryDAG_Assemble_OverBudgetWithSummariesAndSystem(t *testing.T) {
	// Force the trim branch by stuffing the conversation past the budget
	// and seeding both d0 and d2 summaries — the assembled context must
	// keep the system message, append the historical-context block, and
	// trim recent messages by token weight.
	store := NewInMemoryStore()
	defer store.Close()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}

	cfg := DefaultDAGConfig()
	dag := NewSummaryDAG(summaryStore, store, &mockSummaryLLM{}, cfg, &EstimateCounter{})

	ctx := context.Background()
	convID := "trimmed"
	bigText := strings.Repeat("alpha bravo ", 20)
	msgs := []model.Message{model.NewTextMessage(model.RoleSystem, "you are helper")}
	for i := 0; i < 30; i++ {
		msgs = append(msgs, model.NewTextMessage(model.RoleUser, bigText))
		msgs = append(msgs, model.NewTextMessage(model.RoleAssistant, bigText))
	}
	_ = store.SaveMessages(ctx, convID, msgs)

	// Seed summary nodes that match earlier seqs so the historical
	// context block is rendered (covers the depth>=2 + depth<=1 zones).
	_ = summaryStore.Save(ctx, &SummaryNode{
		ConversationID: convID, Depth: 0, Content: "early-d0", EarliestSeq: 1, LatestSeq: 5, TokenCount: 5, ExpandHint: "[Expand for details about: early]",
	})
	_ = summaryStore.Save(ctx, &SummaryNode{
		ConversationID: convID, Depth: 2, Content: "early-d2", EarliestSeq: 1, LatestSeq: 5, TokenCount: 8,
	})

	got, err := dag.Assemble(ctx, convID, 200) // tight budget forces trim
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected non-empty assembled context")
	}
	// System must lead and contain the historical context block.
	if got[0].Role != model.RoleSystem {
		t.Fatalf("expected system to lead, got role=%s", got[0].Role)
	}
	if !strings.Contains(got[0].Content(), "[Historical context]") {
		t.Fatalf("expected historical context block in system, got %q", got[0].Content())
	}
	// And the result must be smaller than the raw transcript.
	if len(got) >= len(msgs) {
		t.Fatalf("expected trim to drop messages, got %d (in=%d)", len(got), len(msgs))
	}
}

func TestSummaryDAG_Assemble_OverBudgetWithoutSystem(t *testing.T) {
	// No leading system message: exercises the "synthetic system from
	// historical context only" branch.
	store := NewInMemoryStore()
	defer store.Close()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}

	dag := NewSummaryDAG(summaryStore, store, &mockSummaryLLM{}, DefaultDAGConfig(), &EstimateCounter{})

	ctx := context.Background()
	convID := "no-system"
	bigText := strings.Repeat("xyz ", 80)
	var msgs []model.Message
	for i := 0; i < 25; i++ {
		msgs = append(msgs, model.NewTextMessage(model.RoleUser, bigText))
	}
	_ = store.SaveMessages(ctx, convID, msgs)

	_ = summaryStore.Save(ctx, &SummaryNode{
		ConversationID: convID, Depth: 0, Content: "summary-of-old", EarliestSeq: 0, LatestSeq: 10, TokenCount: 5,
	})

	got, err := dag.Assemble(ctx, convID, 150)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected non-empty result")
	}
	// First message should be the synthesized system that wraps the
	// historical context (since no system existed in the input).
	if got[0].Role != model.RoleSystem {
		t.Fatalf("expected synthesized system message, got role=%s", got[0].Role)
	}
}
