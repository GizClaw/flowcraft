package history

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
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

func TestLosslessMemory_SaveAndLoad(t *testing.T) {
	store := NewInMemoryStore()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	ml := &mockSummaryLLM{}
	dag := NewSummaryDAG(summaryStore, store, ml, DefaultDAGConfig(), &EstimateCounter{})
	mem := NewLosslessMemory(store, dag, DefaultDAGConfig(), nil, "")

	ctx := context.Background()
	convID := "test-conv-1"

	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, "You are a helper"),
		llm.NewTextMessage(llm.RoleUser, "Hello"),
		llm.NewTextMessage(llm.RoleAssistant, "Hi there"),
	}

	if err := mem.Save(ctx, convID, msgs); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Wait for async ingest.
	time.Sleep(200 * time.Millisecond)

	loaded, err := mem.Load(ctx, convID)
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

func TestLosslessMemory_Clear(t *testing.T) {
	store := NewInMemoryStore()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	ml := &mockSummaryLLM{}
	dag := NewSummaryDAG(summaryStore, store, ml, DefaultDAGConfig(), &EstimateCounter{})
	mem := NewLosslessMemory(store, dag, DefaultDAGConfig(), nil, "")

	ctx := context.Background()
	convID := "test-clear"

	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "Hello"),
	}
	_ = mem.Save(ctx, convID, msgs)

	if err := mem.Clear(ctx, convID); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	loaded, err := mem.Load(ctx, convID)
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

func TestLosslessMemory_CloseWaitsForAsync(t *testing.T) {
	store := NewInMemoryStore()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	ml := &mockSummaryLLM{}
	dag := NewSummaryDAG(summaryStore, store, ml, DefaultDAGConfig(), &EstimateCounter{})
	mem := NewLosslessMemory(store, dag, DefaultDAGConfig(), nil, "")

	ctx := context.Background()

	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "Hello"),
		llm.NewTextMessage(llm.RoleAssistant, "Hi"),
	}

	for i := 0; i < 5; i++ {
		if err := mem.Save(ctx, fmt.Sprintf("conv-%d", i), msgs); err != nil {
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

func TestLosslessMemory_SemaphoreFull(t *testing.T) {
	store := NewInMemoryStore()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	slowLLM := &slowMockLLM{delay: 500 * time.Millisecond}
	dag := NewSummaryDAG(summaryStore, store, slowLLM, DefaultDAGConfig(), &EstimateCounter{})
	mem := NewLosslessMemory(store, dag, DefaultDAGConfig(), nil, "")
	// Override semaphore to capacity 1 for testing.
	mem.sem = make(chan struct{}, 1)

	ctx := context.Background()
	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleUser, "Hello"),
		llm.NewTextMessage(llm.RoleAssistant, "Hi"),
	}

	// Save multiple conversations rapidly; only 1 can ingest concurrently.
	for i := 0; i < 5; i++ {
		if err := mem.Save(ctx, fmt.Sprintf("conv-%d", i), msgs); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}

	mem.Close()

	// All conversations should have messages saved (sync part of Save).
	for i := 0; i < 5; i++ {
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
