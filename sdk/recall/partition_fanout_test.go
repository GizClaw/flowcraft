package recall_test

import (
	"context"
	"sort"
	"testing"

	"github.com/GizClaw/flowcraft/memory/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdk/recall"
)

// TestRecall_MultiPartitionFansOutAcrossNamespaces pins issue #150:
// setting Partitions=[PartitionUser, PartitionGlobal] on a user-
// scoped query must union the per-user bucket AND the global
// bucket. Pre-fix Recall ignored Scope.Partitions entirely and
// only visited NamespaceFor(scope) — so a documented capability
// was silently broken.
func TestRecall_MultiPartitionFansOutAcrossNamespaces(t *testing.T) {
	idx := memory.New()
	mem, err := recall.New(idx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })

	ctx := context.Background()
	userScope := recall.Scope{RuntimeID: "rt", UserID: "alice"}
	globalScope := recall.Scope{RuntimeID: "rt"}

	// Seed: one fact in each bucket. Both mention "coffee" so a
	// query for it should retrieve from each namespace
	// individually under single-partition recall.
	mustAdd(t, mem, ctx, userScope, recall.Entry{
		Content: "alice loves espresso coffee",
		Subject: "alice", Predicate: "drink_preference",
	})
	mustAdd(t, mem, ctx, globalScope, recall.Entry{
		Content: "the office stocks fairtrade coffee",
		Subject: "office", Predicate: "supply",
	})

	// Default user-scope recall sees only the per-user fact.
	defaultHits := mustRecall(t, mem, ctx, userScope, recall.Request{
		Query: "coffee",
		TopK:  10,
	})
	defaultIDs := hitIDs(defaultHits)
	if !contains(defaultIDs, "alice/drink_preference") {
		t.Fatalf("default recall must see alice's fact; got %v", defaultIDs)
	}
	if contains(defaultIDs, "office/supply") {
		t.Fatalf("default recall must NOT see global fact (only user bucket); got %v", defaultIDs)
	}

	// Multi-partition recall unions both buckets.
	multiScope := userScope
	multiScope.Partitions = []recall.Partition{recall.PartitionUser, recall.PartitionGlobal}
	multiHits := mustRecall(t, mem, ctx, multiScope, recall.Request{
		Query: "coffee",
		TopK:  10,
	})
	multiIDs := hitIDs(multiHits)
	if !contains(multiIDs, "alice/drink_preference") {
		t.Fatalf("#150 multi-partition recall must see alice's fact; got %v", multiIDs)
	}
	if !contains(multiIDs, "office/supply") {
		t.Fatalf("#150 multi-partition recall must see global fact; got %v", multiIDs)
	}
}

// TestRecall_MultiPartition_DedupesByDocIDKeepingMaxScore covers
// the merge contract: if the same Doc.ID appears in two
// partitions (e.g. operator-replicated row), the merged result
// keeps the higher-scored copy, not two duplicates.
func TestRecall_MultiPartition_DedupesByDocIDKeepingMaxScore(t *testing.T) {
	idx := memory.New()
	mem, err := recall.New(idx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })

	ctx := context.Background()
	userScope := recall.Scope{RuntimeID: "rt", UserID: "alice"}
	globalScope := recall.Scope{RuntimeID: "rt"}

	sharedID := "shared-entry-1"
	mustAdd(t, mem, ctx, userScope, recall.Entry{
		ID: sharedID, Content: "shared fact about coffee",
		Subject: "_", Predicate: "_",
	})
	mustAdd(t, mem, ctx, globalScope, recall.Entry{
		ID: sharedID, Content: "shared fact about coffee",
		Subject: "_", Predicate: "_",
	})

	multiScope := userScope
	multiScope.Partitions = []recall.Partition{recall.PartitionUser, recall.PartitionGlobal}
	hits := mustRecall(t, mem, ctx, multiScope, recall.Request{
		Query: "coffee", TopK: 10,
	})
	count := 0
	for _, h := range hits {
		if h.Entry.ID == sharedID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("#150 dedup: same Doc.ID across partitions must collapse to one hit; got %d copies of %q", count, sharedID)
	}
}

// TestRecall_MultiPartition_RespectsTopKAfterMerge: the merge
// truncates to TopK from the combined pool, not per-partition.
func TestRecall_MultiPartition_RespectsTopKAfterMerge(t *testing.T) {
	idx := memory.New()
	mem, err := recall.New(idx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })

	ctx := context.Background()
	userScope := recall.Scope{RuntimeID: "rt", UserID: "alice"}
	globalScope := recall.Scope{RuntimeID: "rt"}
	for i := 0; i < 3; i++ {
		mustAdd(t, mem, ctx, userScope, recall.Entry{
			ID:      "user-" + string(rune('A'+i)),
			Content: "alice coffee fact " + string(rune('A'+i)),
			Subject: "alice", Predicate: "pref",
		})
		mustAdd(t, mem, ctx, globalScope, recall.Entry{
			ID:      "global-" + string(rune('A'+i)),
			Content: "shared coffee fact " + string(rune('A'+i)),
			Subject: "office", Predicate: "supply",
		})
	}
	multiScope := userScope
	multiScope.Partitions = []recall.Partition{recall.PartitionUser, recall.PartitionGlobal}
	hits := mustRecall(t, mem, ctx, multiScope, recall.Request{
		Query: "coffee", TopK: 4,
	})
	if len(hits) > 4 {
		t.Fatalf("#150 TopK: merged result must be truncated to TopK=4; got %d", len(hits))
	}
	// Scores must be descending (merge sort contract).
	for i := 1; i < len(hits); i++ {
		if hits[i-1].Score < hits[i].Score {
			t.Fatalf("merge ordering violated: %v", hits)
		}
	}
}

// --- helpers -----------------------------------------------------

func mustAdd(t *testing.T, mem recall.Memory, ctx context.Context, scope recall.Scope, e recall.Entry) {
	t.Helper()
	if e.ID == "" {
		e.ID = e.Subject + "/" + e.Predicate
	}
	if _, err := mem.Add(ctx, scope, e); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

func mustRecall(t *testing.T, mem recall.Memory, ctx context.Context, scope recall.Scope, req recall.Request) []recall.Hit {
	t.Helper()
	hits, err := mem.Recall(ctx, scope, req)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	return hits
}

func hitIDs(hits []recall.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Entry.ID)
	}
	sort.Strings(out)
	return out
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
