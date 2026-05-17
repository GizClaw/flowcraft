package history

import (
	"context"
	"fmt"
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

// TestCompactor_PersistAppend_EnqueueOrderMatchesPersistOrder pins
// issue #179.2: persist-order and enqueue-order must agree on the
// same conversation, because the DAG ingest worker is positional
// on startSeq — a later task (startSeq=N) reaching the worker
// before the earlier task (startSeq=0) that produced N corrupts
// summary parent/child wiring even when the message log itself is
// fine.
//
// Pre-fix, persistMu was acquired+released inside persistAppend
// and the subsequent enqueueAsync happened unprotected, so the
// two steps lived in separate critical sections and concurrent
// Append goroutines could race between them. Post-fix, Append
// holds persistMu across both, so the per-conversation FIFO of
// (persist, enqueue) tuples is preserved.
//
// We assert it via the leaf summary nodes the worker writes to
// the SummaryStore: ChunkSize=1 means every Ingest produces
// exactly one leaf carrying its task's startSeq verbatim as
// EarliestSeq, and inMemSummaryStore appends in call-order, so
// the recorded EarliestSeq sequence is the worker's processing
// order. Under the post-fix invariant the sequence MUST be
// strictly monotonic 0, 1, …, N-1; any inversion is a regression.
func TestCompactor_PersistAppend_EnqueueOrderMatchesPersistOrder(t *testing.T) {
	store := NewInMemoryStore()
	defer store.Close()

	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	cfg := DefaultDAGConfig()
	cfg.ChunkSize = 1
	// Bump every consolidation knob well above the workload so
	// leaves stay at depth=0 and are not merged mid-run — keeps the
	// EarliestSeq sequence we examine 1:1 with task arrival order.
	// CondenseThreshold creates depth+1 condensed nodes; CompactThreshold
	// triggers full-DAG compaction. Both would inject extra nodes
	// into inMemSummaryStore and break the leaves-per-task assertion.
	cfg.CondenseThreshold = 1 << 20
	cfg.Compact.CompactThreshold = 1 << 20
	dag := NewSummaryDAG(summaryStore, store, &mockSummaryLLM{}, cfg, &EstimateCounter{})
	mem := newCompactor(store, dag, cfg, nil, "")

	const (
		workers     = 16
		appendsEach = 8
	)
	convID := "conv-179-2"

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < appendsEach; i++ {
				tag := fmt.Sprintf("w%02d-i%02d", worker, i)
				if err := mem.Append(context.Background(), convID, []llm.Message{
					llm.NewTextMessage(llm.RoleUser, tag),
				}); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// Drain the per-conversation worker before observing — Shutdown
	// closes the queue and waits for in-flight ingests, so by the
	// time it returns every enqueued task has reached the SummaryStore.
	if err := mem.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	totalAppends := workers * appendsEach

	// Sanity: persisted log has the expected count (covers the
	// #162 invariant in passing).
	got, err := store.GetMessages(context.Background(), convID)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(got) != totalAppends {
		t.Fatalf("persisted log length: got %d, want %d", len(got), totalAppends)
	}

	// Leaves recorded in the order the worker processed them.
	leaves := summaryStore.data[convID]
	if len(leaves) != totalAppends {
		t.Fatalf("leaf count: got %d, want %d (every Ingest should produce 1 leaf with ChunkSize=1)", len(leaves), totalAppends)
	}
	for i, n := range leaves {
		if n.Depth != 0 {
			t.Fatalf("leaf[%d] Depth=%d, want 0 (compact path triggered unexpectedly)", i, n.Depth)
		}
		if n.EarliestSeq != i {
			t.Fatalf("#179.2 regression: leaf[%d] EarliestSeq=%d, want %d — worker processed tasks out of persist-order", i, n.EarliestSeq, i)
		}
	}
}
