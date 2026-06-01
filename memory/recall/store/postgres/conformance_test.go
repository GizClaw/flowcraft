package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/recalltest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTemporalStoreConformance(t *testing.T) {
	requirePostgres(t)
	recalltest.RunTemporalStoreSuite(t, func(t testing.TB) recall.TemporalStore {
		return newTestBackend(t).TemporalStore()
	})
	recalltest.RunScopeEnumeratorSuite(t, func(t testing.TB) (recall.TemporalStore, recall.ScopeEnumerator) {
		store := newTestBackend(t).TemporalStore()
		return store, store
	})
}

func TestSideEffectOutboxConformance(t *testing.T) {
	requirePostgres(t)
	recalltest.RunSideEffectOutboxSuite(t, func(t testing.TB) recall.SideEffectOutbox {
		return newTestBackend(t).SideEffectOutbox()
	})
}

func TestAsyncSemanticQueueConformance(t *testing.T) {
	requirePostgres(t)
	recalltest.RunAsyncSemanticQueueSuite(t, func(t testing.TB) recall.AsyncSemanticQueue {
		return newTestBackend(t).AsyncSemanticQueue()
	})
}

func TestEvidenceStoreConformance(t *testing.T) {
	requirePostgres(t)
	recalltest.RunEvidenceStoreSuite(t, func(t testing.TB) recall.EvidenceStore {
		return newTestBackend(t).EvidenceStore()
	})
}

func TestGraphStoreConformance(t *testing.T) {
	requirePostgres(t)
	recalltest.RunObservationStoreSuite(t, func(t testing.TB) recall.ObservationStore {
		return newTestBackend(t).ObservationStore()
	})
	recalltest.RunLinkStoreSuite(t, func(t testing.TB) recall.LinkStore {
		return newTestBackend(t).LinkStore()
	})
}

func newTestBackend(t testing.TB) *Backend {
	t.Helper()
	b, err := Open(context.Background(), os.Getenv("FC_PG_DSN"))
	if err != nil {
		t.Fatalf("open postgres recall store: %v", err)
	}
	if err := resetTables(context.Background(), os.Getenv("FC_PG_DSN")); err != nil {
		_ = b.Close()
		t.Fatalf("reset postgres recall store: %v", err)
	}
	t.Cleanup(func() {
		if err := resetTables(context.Background(), os.Getenv("FC_PG_DSN")); err != nil {
			t.Fatalf("reset postgres recall store cleanup: %v", err)
		}
		if err := b.Close(); err != nil {
			t.Fatalf("close postgres recall store: %v", err)
		}
	})
	return b
}

func resetTables(ctx context.Context, dsn string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	for _, stmt := range []string{
		`DELETE FROM recall_async_semantic_job_episodes`,
		`DELETE FROM recall_async_semantic_jobs`,
		`DELETE FROM recall_side_effect_jobs`,
		`DELETE FROM recall_queue_counters`,
		`DELETE FROM recall_links`,
		`DELETE FROM recall_observations`,
		`DELETE FROM recall_evidence_refs`,
		`DELETE FROM recall_fact_entities`,
		`DELETE FROM recall_facts`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func requirePostgres(t testing.TB) {
	t.Helper()
	if os.Getenv("FC_PG_DSN") == "" {
		t.Skip("FC_PG_DSN not set; skipping postgres recall store tests")
	}
}
