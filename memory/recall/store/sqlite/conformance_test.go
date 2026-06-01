package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/recalltest"
)

func TestTemporalStoreConformance(t *testing.T) {
	recalltest.RunTemporalStoreSuite(t, func(t testing.TB) recall.TemporalStore {
		return newTestBackend(t).TemporalStore()
	})
	recalltest.RunScopeEnumeratorSuite(t, func(t testing.TB) (recall.TemporalStore, recall.ScopeEnumerator) {
		store := newTestBackend(t).TemporalStore()
		return store, store
	})
}

func TestSideEffectOutboxConformance(t *testing.T) {
	recalltest.RunSideEffectOutboxSuite(t, func(t testing.TB) recall.SideEffectOutbox {
		return newTestBackend(t).SideEffectOutbox()
	})
}

func TestAsyncSemanticQueueConformance(t *testing.T) {
	recalltest.RunAsyncSemanticQueueSuite(t, func(t testing.TB) recall.AsyncSemanticQueue {
		return newTestBackend(t).AsyncSemanticQueue()
	})
}

func TestEvidenceStoreConformance(t *testing.T) {
	recalltest.RunEvidenceStoreSuite(t, func(t testing.TB) recall.EvidenceStore {
		return newTestBackend(t).EvidenceStore()
	})
}

func TestGraphStoreConformance(t *testing.T) {
	recalltest.RunObservationStoreSuite(t, func(t testing.TB) recall.ObservationStore {
		return newTestBackend(t).ObservationStore()
	})
	recalltest.RunLinkStoreSuite(t, func(t testing.TB) recall.LinkStore {
		return newTestBackend(t).LinkStore()
	})
}

func newTestBackend(t testing.TB) *Backend {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recall.db")
	b, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open sqlite recall store: %v", err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Fatalf("close sqlite recall store: %v", err)
		}
	})
	return b
}
