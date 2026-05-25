package workspace

import (
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

func TestBackendPersistsAcrossReopen(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "recall")
	b, err := Open(dir)
	if err != nil {
		t.Fatalf("open workspace recall store: %v", err)
	}
	scope := recall.Scope{RuntimeID: "rt", UserID: "u1"}
	fact := recall.TemporalFact{ID: "f1", Scope: scope, Kind: recall.FactNote, Content: "durable"}
	if err := b.TemporalStore().Append(ctx, []recall.TemporalFact{fact}); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen workspace recall store: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Fatalf("close workspace recall store: %v", err)
		}
	})
	got, err := reopened.TemporalStore().Get(ctx, scope, "f1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "durable" {
		t.Fatalf("reopened fact content = %q, want durable", got.Content)
	}
}

func newTestBackend(t testing.TB) *Backend {
	t.Helper()
	b, err := Open(filepath.Join(t.TempDir(), "workspace"))
	if err != nil {
		t.Fatalf("open workspace recall store: %v", err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Fatalf("close workspace recall store: %v", err)
		}
	})
	return b
}
