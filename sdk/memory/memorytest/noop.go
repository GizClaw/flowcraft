package memorytest

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/memory"
)

// NoopSuite drives every Run* suite against NoopRuntime. An
// impl that has trouble passing RunNoop has a problem with
// the kernel, not with the individual sub-tests.
//
// NoopRuntime does not persist anything, so the load-pagination
// and idempotency sub-tests are skipped (they have no
// meaningful behaviour against a noop). All other sub-tests
// run.
func RunNoop(t *testing.T) {
	t.Helper()
	spec := memory.Spec{RuntimeID: "noop"}
	scope := memory.Scope{RuntimeID: spec.RuntimeID, UserID: "u"}
	conv := "conv-noop"

	buildRT := func(t *testing.T) *memory.Runtime {
		t.Helper()
		noop, err := memory.NewNoopRuntime(spec)
		if err != nil {
			t.Fatalf("NewNoopRuntime: %v", err)
		}
		return noop.Runtime
	}

	// Scope: every assertion in RunScope is impl-free.
	RunScope(t, ScopeSuite{})

	// Append: skip the idempotency and monotonicity sub-tests
	// by writing a slimmed-down version that does not depend on
	// persistence.
	t.Run("Append", func(t *testing.T) {
		rt := buildRT(t)
		resp, err := rt.ExecuteAppend(t.Context(), memory.AppendRequest{
			Scope:          scope,
			ConversationID: conv,
			Records:        []memory.Record{mustRecord("r-1", "hi")},
		})
		if err != nil {
			t.Fatalf("ExecuteAppend: %v", err)
		}
		if resp.Appended != 0 {
			t.Errorf("NoopRuntime Appended = %d, want 0", resp.Appended)
		}
	})

	// Load: NoopRuntime returns zero records, so we only assert
	// the round-trip succeeds and the kernel still rejects
	// unbounded loads.
	t.Run("Load", func(t *testing.T) {
		rt := buildRT(t)
		resp, err := rt.ExecuteLoad(t.Context(), memory.LoadRequest{
			Scope:          scope,
			ConversationID: conv,
			Limit:          10,
		})
		if err != nil {
			t.Fatalf("ExecuteLoad: %v", err)
		}
		if len(resp.Records) != 0 {
			t.Errorf("NoopRuntime Records = %d, want 0", len(resp.Records))
		}

		_, err = rt.ExecuteLoad(t.Context(), memory.LoadRequest{
			Scope:          scope,
			ConversationID: conv,
		})
		memErr := memory.AsError(err)
		if memErr == nil || memErr.Kind != memory.KindInvalidRequest {
			t.Errorf("expected KindInvalidRequest for unbounded Load, got: %v", err)
		}
	})

	// Recall: NoopRuntime returns zero hits.
	t.Run("Recall", func(t *testing.T) {
		rt := buildRT(t)
		resp, err := rt.ExecuteRecall(t.Context(), memory.RecallRequest{
			Scope: scope,
			Query: "x",
			TopK:  5,
		})
		if err != nil {
			t.Fatalf("ExecuteRecall: %v", err)
		}
		if len(resp.Hits) != 0 {
			t.Errorf("NoopRuntime Hits = %d, want 0", len(resp.Hits))
		}
	})

	// Import: NoopRuntime returns zero DocumentID/ChunkCount.
	t.Run("Import", func(t *testing.T) {
		rt := buildRT(t)
		resp, err := rt.ExecuteImport(t.Context(), memory.ImportRequest{
			Scope:  scope,
			Source: "memory://x",
		})
		if err != nil {
			t.Fatalf("ExecuteImport: %v", err)
		}
		if resp.DocumentID != "" {
			t.Errorf("NoopRuntime DocumentID = %q, want empty", resp.DocumentID)
		}
	})

	// Compact / Archive: NoopRuntime returns zero counts and no error.
	for _, op := range []string{"Compact", "Archive"} {
		t.Run(op, func(t *testing.T) {
			rt := buildRT(t)
			switch op {
			case "Compact":
				_, err := rt.ExecuteCompact(t.Context(), memory.CompactRequest{
					Scope:     scope,
					OlderThan: time.Now().Add(-time.Hour),
				})
				if err != nil {
					t.Errorf("ExecuteCompact: %v", err)
				}
			case "Archive":
				_, err := rt.ExecuteArchive(t.Context(), memory.ArchiveRequest{
					Scope:       scope,
					OlderThan:   time.Now().Add(-time.Hour),
					Destination: "memory://archive",
				})
				if err != nil {
					t.Errorf("ExecuteArchive: %v", err)
				}
			}
		})
	}
}
