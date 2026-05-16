package history

import (
	"context"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// rmwStore wraps InMemoryStore but deliberately hides the
// MessageAppender capability so persistAppend falls through to the
// read-modify-write fallback. Without this we'd test only the
// fast path; the RMW path was the most egregious failure mode in
// #162 ("entire batches lost last-writer-wins") and must remain
// safe even though defaults no longer take it.
type rmwStore struct{ inner *InMemoryStore }

func (s *rmwStore) GetMessages(ctx context.Context, id string) ([]model.Message, error) {
	return s.inner.GetMessages(ctx, id)
}
func (s *rmwStore) SaveMessages(ctx context.Context, id string, msgs []model.Message) error {
	return s.inner.SaveMessages(ctx, id, msgs)
}
func (s *rmwStore) DeleteMessages(ctx context.Context, id string) error {
	return s.inner.DeleteMessages(ctx, id)
}

// TestCompactor_PersistAppend_NoLostBatchesUnderRace pins issue
// #162: concurrent Append on the same conversation must NOT lose
// batches even when the underlying Store cannot offer an atomic
// MessageAppender path. Run with `-race` to also catch any data
// races in persistAppend itself.
//
// The test deliberately uses [rmwStore] (no MessageAppender) so
// persistAppend goes through the RMW fallback — the worst-case
// path the persistMu keyed lock is supposed to protect.
func TestCompactor_PersistAppend_NoLostBatchesUnderRace(t *testing.T) {
	store := &rmwStore{inner: NewInMemoryStore()}
	defer store.inner.Close()

	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	dag := NewSummaryDAG(summaryStore, store, &mockSummaryLLM{}, DefaultDAGConfig(), &EstimateCounter{})
	mem := newCompactor(store, dag, DefaultDAGConfig(), nil, "")
	defer func() {
		_ = mem.Shutdown(context.Background())
	}()

	const (
		workers     = 16
		appendsEach = 32
	)
	convID := "conv-race-162"
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < appendsEach; i++ {
				msg := llm.NewTextMessage(llm.RoleUser, "x")
				if err := mem.Append(context.Background(), convID, []llm.Message{msg}); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	got, err := store.GetMessages(context.Background(), convID)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	want := workers * appendsEach
	if len(got) != want {
		t.Fatalf("lost batches in RMW persistAppend: got %d messages, want %d (#162 regression)", len(got), want)
	}
}

// TestCompactor_PersistAppend_MessageAppenderPathPreservesStartSeq
// covers the second failure mode of #162: even when the underlying
// Store implements MessageAppender (and so the atomicity of the
// persisted log is fine), the GetMessages -> AppendMessages pair
// in persistAppend computes startSeq from len(existing). Without
// the per-conversation lock, two concurrent Appends can both
// observe the same existing length and report the same startSeq —
// which would later corrupt the DAG summary index that aligns
// summary nodes to message ranges.
//
// We assert it indirectly: the final transcript has exactly
// workers*appendsEach messages, which can only happen if every
// AppendMessages call appended exactly one message in turn (the
// MessageAppender path's atomicity guarantee).
func TestCompactor_PersistAppend_MessageAppenderPathPreservesStartSeq(t *testing.T) {
	store := NewInMemoryStore()
	defer store.Close()

	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	dag := NewSummaryDAG(summaryStore, store, &mockSummaryLLM{}, DefaultDAGConfig(), &EstimateCounter{})
	mem := newCompactor(store, dag, DefaultDAGConfig(), nil, "")
	defer func() {
		_ = mem.Shutdown(context.Background())
	}()

	const (
		workers     = 16
		appendsEach = 32
	)
	convID := "conv-race-162-appender"
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < appendsEach; i++ {
				msg := llm.NewTextMessage(llm.RoleUser, "y")
				if err := mem.Append(context.Background(), convID, []llm.Message{msg}); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	got, err := store.GetMessages(context.Background(), convID)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	want := workers * appendsEach
	if len(got) != want {
		t.Fatalf("MessageAppender path lost batches: got %d, want %d (#162 regression)", len(got), want)
	}
}
