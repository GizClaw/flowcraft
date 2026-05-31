package recall_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdk/recall"
)

// TestAdd_ContentAddressableUnderRetry pins the contract for
// issue #155: Memory.Add is content-addressable when e.ID is
// empty. Two Adds with the same (scope, Content) at different
// wall-clock times must:
//
//  1. produce the same ID (no timestamp in the ID seed), AND
//  2. be deduplicated via the content-hash probe so the second
//     Add returns the existing entry's ID without writing a
//     duplicate row.
//
// Pre-fix Memory.Add stamped RFC3339Nano into the ID seed, so
// retries against transient errors produced duplicate rows. The
// fix makes Add safe under operational retry semantics —
// matching the documented contract.
func TestAdd_ContentAddressableUnderRetry(t *testing.T) {
	idx := memory.New()
	mem, err := recall.New(idx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })

	ctx := context.Background()
	scope := recall.Scope{RuntimeID: "rt-test", UserID: "u-test"}
	entry := recall.Entry{Content: "the same payload across retries"}

	id1, err := mem.Add(ctx, scope, entry)
	if err != nil {
		t.Fatalf("first Add: %v", err)
	}
	// Drive the wall clock forward to prove the contract is
	// time-independent.
	time.Sleep(2 * time.Millisecond)
	id2, err := mem.Add(ctx, scope, entry)
	if err != nil {
		t.Fatalf("second Add: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("#155: Add must be content-addressable under retry; got distinct IDs %q vs %q", id1, id2)
	}
	// And only ONE row landed in the index, not two.
	ns := recall.NamespaceFor(scope)
	resp, err := idx.List(ctx, ns, retrieval.ListRequest{PageSize: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("#155: dedup probe should keep exactly 1 row, got %d", len(resp.Items))
	}
}
