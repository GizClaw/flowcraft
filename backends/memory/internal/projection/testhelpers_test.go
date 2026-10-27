package projection

import (
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// newTestWorkspace returns an isolated local workspace for one test.
func newTestWorkspace(t *testing.T) workspace.Workspace {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

// testShard is the one shard the store's own tests write. They pin the base and
// segment protocol -- chaining, compaction, residency, reclaim -- which lives
// below the shard boundary, so one shard is enough; the routing tests cover what
// happens above it.
const testShard = "c:test"

// testAddress names that shard within a scope.
func testAddress(scope corememory.Scope) Address {
	return Address{Scope: scope, Shard: testShard}
}

// testEntryKey names a test entry and the document it belongs to.
func testEntryKey() EntryKey[testEntry] {
	return EntryKey[testEntry]{
		ID: func(value testEntry) string { return value.ID },
		Document: func(value testEntry) component.DocumentAddress {
			return component.DocumentAddress{DatasetID: value.DatasetID, DocumentID: value.DocumentID}
		},
	}
}

// budgetedStore is a store whose bases are weighed by their entries, so a test
// can put it under a byte budget it can count.
func budgetedStore(t *testing.T, kv storage.Store, maxBytes int64) *Store[testBase, EntryDelta[testEntry]] {
	t.Helper()
	store, err := NewTypedStore(kv, "test", TypedOptions[testBase, EntryDelta[testEntry]]{
		Thresholds: Thresholds{MaxSegments: 4096, MinDeltaBytes: 1 << 20},
		MaxBytes:   maxBytes,
		BaseBytes:  func(base testBase) int64 { return int64(len(base.Entries)) * testEntryBytes },
		Canonicalize: func(delta EntryDelta[testEntry]) EntryDelta[testEntry] {
			return CanonicalEntryDelta(delta, testEntryKey())
		},
		Apply: func(base *testBase, delta EntryDelta[testEntry]) error {
			base.Entries = ApplyEntryDelta(base.Entries, delta, testEntryKey())
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// testEntryBytes is what one entry of a test base counts in the budget.
const testEntryBytes = 100
