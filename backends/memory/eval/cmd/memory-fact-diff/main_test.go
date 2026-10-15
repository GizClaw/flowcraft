package main

import (
	"context"
	"strings"
	"testing"

	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

func batchOf(key string, texts ...string) commit {
	group := commit{key: key}
	for index, text := range texts {
		group.facts = append(group.facts, factview.Fact{
			ID:             key + "/" + string(rune('a'+index)),
			CanonicalHash:  text,
			Text:           text,
			Provenance:     []corememory.SourceRef{{Kind: "raw_message", ID: key, Revision: "1"}},
			ConversationID: "conv",
		})
	}
	return group
}

// TestCompareBatchPairsAClaimAcrossTwoDerivations keeps the reading's unit: two
// derivations of one commit are compared claim by claim, so a fact that came
// back in other words is not reported as gone -- and a fact only one side wrote
// is, with the closest thing the other side has beside it.
func TestCompareBatchPairsAClaimAcrossTwoDerivations(t *testing.T) {
	baseline := batchOf("m1",
		"Caroline went to an LGBTQ support group on 7 May 2023.",
		"Melanie painted a sunrise over the lake.")
	candidate := batchOf("m1",
		"Caroline attended an LGBTQ support group on 7 May 2023.",
		"Melanie is a painter.")
	measured := compareBatch(baseline, candidate, 0.7)
	if measured.Matched != 1 {
		t.Fatalf("matched = %d, want 1 (the reworded fact)", measured.Matched)
	}
	if len(measured.Removed) != 1 || !strings.Contains(measured.Removed[0].Text, "sunrise") {
		t.Fatalf("removed = %v, want the lake fact", measured.Removed)
	}
	if len(measured.Added) != 1 || !strings.Contains(measured.Added[0].Text, "painter") {
		t.Fatalf("added = %v, want the painter fact", measured.Added)
	}
	if measured.Removed[0].Best >= 0.7 {
		t.Fatalf("best similarity = %.2f, want the removed fact to stay under the threshold", measured.Removed[0].Best)
	}
	if !strings.Contains(measured.Removed[0].Closest, "painter") {
		t.Fatalf("closest = %q, want the candidate fact it was measured against", measured.Removed[0].Closest)
	}
}

// TestCompareBatchPairsOneToOne pins the rule that keeps the reading honest: a
// batch's facts are distinct claims, so one candidate fact cannot stand in for
// two baseline facts. A batch that lost a claim reports the loss even though the
// surviving text contains what the lost one said.
func TestCompareBatchPairsOneToOne(t *testing.T) {
	baseline := batchOf("m1",
		"Nadia joined a community choir in Bristol.",
		"Nadia joined a community choir in Bristol and stayed for the whole rehearsal.")
	candidate := batchOf("m1",
		"Nadia joined a community choir in Bristol and stayed for the whole rehearsal.")
	measured := compareBatch(baseline, candidate, 0.7)
	if measured.Matched != 1 {
		t.Fatalf("matched = %d, want 1", measured.Matched)
	}
	if len(measured.Removed) != 1 {
		t.Fatalf("removed = %d facts, want the claim the candidate dropped", len(measured.Removed))
	}
	if len(measured.Added) != 0 {
		t.Fatalf("added = %v, want nothing", measured.Added)
	}
}

// TestGroupByCommitReadsTheBatchesTheExtractorWrote pins the batch key: facts of
// one commit are one batch, and a fact without provenance stands alone instead
// of joining every other provenance-less fact.
func TestGroupByCommitReadsTheBatchesTheExtractorWrote(t *testing.T) {
	ref := func(id string) []corememory.SourceRef {
		return []corememory.SourceRef{{Kind: "raw_message", ID: id, Revision: "1"}}
	}
	groups := groupByCommit([]factview.Fact{
		{ID: "f1", Provenance: ref("m1")},
		{ID: "f2", Provenance: ref("m1")},
		{ID: "f3", Provenance: ref("m2")},
		{ID: "f4"},
		{ID: "f5"},
	})
	if len(groups) != 4 {
		t.Fatalf("batches = %d, want 4 (two facts of one commit, one of another, two orphans)", len(groups))
	}
	if len(groups[0].facts) != 2 {
		t.Fatalf("largest batch = %d facts, want 2", len(groups[0].facts))
	}
	keys := map[string]bool{}
	for _, group := range groups {
		if keys[group.key] {
			t.Fatalf("batch key %q repeated", group.key)
		}
		keys[group.key] = true
	}
}

// TestCompareRefusesAThresholdOutsideTheMeasure keeps the reading's one knob
// checkable: a threshold outside (0, 1] is a mistake in the invocation, not a
// comparison that silently reports every fact twice.
func TestCompareRefusesAThresholdOutsideTheMeasure(t *testing.T) {
	_, err := compare(context.Background(), "unused", "unused", corememory.Scope{RuntimeID: "memories"}, 0)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("err = %v, want the threshold refused", err)
	}
}

// TestCappedMeasuresTheLeverOnTheBaselineBatches keeps the cap's size honest: it
// counts the batches a cap would cut into and the facts that leaves out, which is
// what the reading has to compare against what the candidate actually dropped.
func TestCappedMeasuresTheLeverOnTheBaselineBatches(t *testing.T) {
	measured := describe([]commit{
		batchOf("m1", "a"), batchOf("m2", "a", "b", "c", "d"), batchOf("m3", "a", "b", "c", "d", "e", "f"),
	})
	batches, facts := measured.capped(4)
	if batches != 1 || facts != 2 {
		t.Fatalf("cap 4 cuts %d batches and %d facts, want 1 batch and 2 facts", batches, facts)
	}
	if batches, facts := measured.capped(0); batches != 0 || facts != 0 {
		t.Fatalf("cap 0 cuts %d batches and %d facts, want the lever off", batches, facts)
	}
}
