package main

import (
	"testing"

	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

func jaccardOf(measured pair) float64 { return measured.jaccard }

func containmentOf(measured pair) float64 { return measured.containment }

// TestGroupByCommitReadsTheBatchesTheExtractorWrote pins the reading's unit: the
// facts that share one source commit are the batch the budget selects over, and a
// fact without provenance stands alone rather than joining every other
// provenance-less fact into one enormous batch.
func TestGroupByCommitReadsTheBatchesTheExtractorWrote(t *testing.T) {
	commit := func(id string) []corememory.SourceRef {
		return []corememory.SourceRef{{Kind: "raw_message", ID: id, Revision: "1"}}
	}
	groups := groupByCommit([]factview.Fact{
		{ID: "f1", Provenance: commit("m1")},
		{ID: "f2", Provenance: commit("m1")},
		{ID: "f3", Provenance: commit("m2")},
		{ID: "f4"},
		{ID: "f5"},
	})
	if len(groups) != 4 {
		t.Fatalf("groups = %d, want 4 (two facts of one commit, one of another, two orphans)", len(groups))
	}
	if len(groups[0].facts) != 2 {
		t.Fatalf("largest batch = %d facts, want 2", len(groups[0].facts))
	}
	keys := map[string]bool{}
	for _, group := range groups {
		if keys[group.key] {
			t.Fatalf("group key %q repeated", group.key)
		}
		keys[group.key] = true
	}
}

// TestMergeProjectsTheExtractorScan keeps the reading's projection honest: it
// applies the same greedy scan the extractor would (compare against the facts
// already kept of the batch, the longer text survives), so the predicted
// survivors are the number the budget would actually leave.
func TestMergeProjectsTheExtractorScan(t *testing.T) {
	facts := []factview.Fact{
		{ID: "a", CanonicalHash: "a", Text: "On 12 March 2023, Nadia joined a community choir in Bristol."},
		{ID: "b", CanonicalHash: "b", Text: "On 12 March 2023, Nadia joined a community choir in Bristol and stayed for the whole rehearsal that evening."},
		{ID: "c", CanonicalHash: "c", Text: "Nadia works as a bookkeeper in Bristol."},
		{ID: "d", CanonicalHash: "d", Text: "Yes."},
	}
	if kept := merge(facts, 0.7, 0, jaccardOf); len(kept) != 4 {
		t.Fatalf("jaccard 0.7 kept %d facts, want the whole batch: a trailing clause is not similarity", len(kept))
	}
	if kept := merge(facts, 0.9, 0, containmentOf); len(kept) != 3 {
		t.Fatalf("containment 0.9 kept %d facts, want the restatement folded", len(kept))
	}
	if kept := merge(facts, 2, 4, jaccardOf); len(kept) != 3 {
		t.Fatalf("floor kept %d facts, want the one-token claim dropped", len(kept))
	}
	// The canonical identity already folds exact duplicates, so the reading must
	// not count them as a pair the budget could merge.
	pairs := pairsOf([]factview.Fact{
		{ID: "a", CanonicalHash: "a", Text: "Nadia joined a choir."},
		{ID: "b", CanonicalHash: "a", Text: "Nadia joined a choir."},
	})
	if len(pairs) != 0 {
		t.Fatalf("pairs = %d, want none between facts of one canonical identity", len(pairs))
	}
}
