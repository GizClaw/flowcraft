package ltm_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/memory/ltm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/retrieval/journal"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

type stubLLM struct {
	resp string
	err  error
}

func (s *stubLLM) Generate(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	if s.err != nil {
		return llm.Message{}, llm.TokenUsage{}, s.err
	}
	return llm.Message{
		Role:  model.RoleAssistant,
		Parts: []model.Part{{Type: model.PartText, Text: s.resp}},
	}, llm.TokenUsage{}, nil
}

func (s *stubLLM) GenerateStream(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("not used")
}

func newScope() memory.MemoryScope {
	return memory.MemoryScope{RuntimeID: "rt1", AgentID: "bot", UserID: "u1", SessionID: "s1"}
}

func TestSaveExtractsAdditiveFacts(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	resp := `[{"content":"用户喜欢黑咖啡","categories":["preference"],"entities":["黑咖啡"],"source":"user","confidence":0.95}]`
	m, err := ltm.New(ltm.Config{
		Index:         idx,
		LLM:           &stubLLM{resp: resp},
		RequireUserID: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	res, err := m.Save(ctx, newScope(), []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "我每天早上喝黑咖啡"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.EntryIDs) != 1 {
		t.Fatalf("entry_ids=%v", res.EntryIDs)
	}
	hits, err := m.Recall(ctx, newScope(), ltm.RecallRequest{Query: "咖啡", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Entry.Content, "黑咖啡") {
		t.Fatalf("hits=%+v", hits)
	}
	if hits[0].Entry.Categories[0] != "preference" {
		t.Fatalf("categories=%+v", hits[0].Entry.Categories)
	}
	if len(hits[0].Entry.Entities) == 0 {
		t.Fatalf("entities lost: %+v", hits[0].Entry)
	}
}

func TestSaveIdempotentByDocID(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	resp := `[{"content":"User likes coffee","entities":["coffee"]}]`
	m, _ := ltm.New(ltm.Config{Index: idx, LLM: &stubLLM{resp: resp}, RequireUserID: true})
	defer m.Close()
	scope := newScope()
	msgs := []llm.Message{{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "I love coffee"}}}}
	r1, err := m.Save(ctx, scope, msgs)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := m.Save(ctx, scope, msgs)
	if err != nil {
		t.Fatal(err)
	}
	if r1.EntryIDs[0] != r2.EntryIDs[0] {
		t.Fatalf("ids drift: %s vs %s", r1.EntryIDs[0], r2.EntryIDs[0])
	}
	hits, _ := m.Recall(ctx, scope, ltm.RecallRequest{Query: "coffee", TopK: 5})
	if len(hits) != 1 {
		t.Fatalf("expected dedup via idempotent upsert, got %d", len(hits))
	}
}

func TestRequireUserIDValidation(t *testing.T) {
	idx := memidx.New()
	m, _ := ltm.New(ltm.Config{Index: idx, LLM: &stubLLM{resp: "[]"}, RequireUserID: true})
	defer m.Close()
	_, err := m.Save(context.Background(), memory.MemoryScope{RuntimeID: "rt1"}, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "hi"}}},
	})
	if !errors.Is(err, ltm.ErrMissingUserID) {
		t.Fatalf("expected ErrMissingUserID, got %v", err)
	}
}

func TestAddRawAndAgentFilter(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	m, _ := ltm.New(ltm.Config{Index: idx, RequireUserID: true})
	defer m.Close()
	scope := newScope()
	if _, err := m.AddRaw(ctx, scope, memory.MemoryEntry{Content: "raw fact for bot1", Categories: []string{"profile"}}); err != nil {
		t.Fatal(err)
	}
	other := scope
	other.AgentID = "other-bot"
	if _, err := m.AddRaw(ctx, other, memory.MemoryEntry{Content: "raw fact for other bot", Categories: []string{"profile"}}); err != nil {
		t.Fatal(err)
	}
	hits, err := m.Recall(ctx, scope, ltm.RecallRequest{Query: "raw fact", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Entry.Content, "bot1") {
		t.Fatalf("agent filter leaked: %+v", hits)
	}
}

func TestTTLSoftFilter(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	now := time.Now()
	m, _ := ltm.New(ltm.Config{Index: idx, RequireUserID: true, Now: func() time.Time { return now }})
	defer m.Close()
	scope := newScope()
	past := now.Add(-time.Second)
	if _, err := m.AddRaw(ctx, scope, memory.MemoryEntry{
		Content: "expired memory", Categories: []string{"episodic"}, ExpiresAt: &past,
	}); err != nil {
		t.Fatal(err)
	}
	future := now.Add(24 * time.Hour)
	if _, err := m.AddRaw(ctx, scope, memory.MemoryEntry{
		Content: "active memory", Categories: []string{"episodic"}, ExpiresAt: &future,
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := m.Recall(ctx, scope, ltm.RecallRequest{Query: "memory", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Entry.Content, "active") {
		t.Fatalf("expected only active memory, got %+v", hits)
	}
	hitsAll, _ := m.Recall(ctx, scope, ltm.RecallRequest{Query: "memory", TopK: 5, WithStale: true})
	if len(hitsAll) != 2 {
		t.Fatalf("WithStale expected 2, got %d", len(hitsAll))
	}
}

func TestSweeperPhysicalDelete(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	now := time.Now()
	m, _ := ltm.New(ltm.Config{
		Index:           idx,
		RequireUserID:   true,
		TTLPolicy:       ltm.CategoryTTLPolicy{memory.CategoryEpisodic: time.Hour},
		SweeperEnabled:  false, // we manually call SweepNamespace
		Now:             func() time.Time { return now },
		SweeperBatchMax: 10,
	})
	defer m.Close()
	scope := newScope()
	past := now.Add(-time.Second)
	id, err := m.AddRaw(ctx, scope, memory.MemoryEntry{Content: "old", Categories: []string{"episodic"}, ExpiresAt: &past})
	if err != nil {
		t.Fatal(err)
	}
	sweeper, ok := m.(interface {
		SweepNamespace(ctx context.Context, ns string) error
	})
	if !ok {
		t.Fatal("Memory does not expose SweepNamespace")
	}
	if err := sweeper.SweepNamespace(ctx, ltm.NamespaceFor(scope)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := idx.Get(ctx, ltm.NamespaceFor(scope), id); ok {
		t.Fatalf("sweeper failed to delete %s", id)
	}
}

func TestAsyncSaveAndAwait(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	resp := `[{"content":"async fact"}]`
	m, _ := ltm.New(ltm.Config{
		Index: idx, LLM: &stubLLM{resp: resp},
		RequireUserID: true,
		AsyncWorkers:  1,
	})
	defer m.Close()
	scope := newScope()
	id, err := m.SaveAsync(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "remember the async fact"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.AwaitJob(ctx, id, 5*time.Second)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if st.State != ltm.JobSucceeded {
		t.Fatalf("state=%s err=%s", st.State, st.LastError)
	}
	if len(st.EntryIDs) != 1 {
		t.Fatalf("expected 1 entry, got %v", st.EntryIDs)
	}
}

func TestHistoryAndRollback(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	j := journal.NewMemoryJournal()
	m, _ := ltm.New(ltm.Config{
		Index: idx, RequireUserID: true, Journal: j,
	})
	defer m.Close()
	scope := newScope()
	t1 := time.Now()
	id, _ := m.AddRaw(ctx, scope, memory.MemoryEntry{Content: "v1", Categories: []string{"profile"}, CreatedAt: t1, UpdatedAt: t1})
	time.Sleep(10 * time.Millisecond)
	t2 := time.Now()
	_, _ = m.AddRaw(ctx, scope, memory.MemoryEntry{ID: id, Content: "v2", Categories: []string{"profile"}, CreatedAt: t2, UpdatedAt: t2})

	events, err := m.History(ctx, scope, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("history len=%d", len(events))
	}

	if err := m.Rollback(ctx, scope, id, t1.Add(5*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	hits, _ := m.Recall(ctx, scope, ltm.RecallRequest{Query: "v1", TopK: 5})
	if len(hits) != 1 || hits[0].Entry.Content != "v1" {
		t.Fatalf("rollback failed: %+v", hits)
	}
}

func TestForget(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	m, _ := ltm.New(ltm.Config{Index: idx, RequireUserID: true})
	defer m.Close()
	scope := newScope()
	id, _ := m.AddRaw(ctx, scope, memory.MemoryEntry{Content: "forget me", Categories: []string{"episodic"}})
	if err := m.Forget(ctx, scope, id, "user_request"); err != nil {
		t.Fatal(err)
	}
	hits, _ := m.Recall(ctx, scope, ltm.RecallRequest{Query: "forget", TopK: 5})
	if len(hits) != 0 {
		t.Fatalf("expected forgotten, got %+v", hits)
	}
}
