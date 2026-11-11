package chat

import "testing"

// TestTruncateFactsCutsByPositionWithoutTheQuotaAndByPriorityWithIt pins the two
// rules side by side: without the quota an over-full batch loses whatever the
// model wrote last, with it the batch loses the facts the extractor itself judged
// unable to stand alone.
func TestTruncateFactsCutsByPositionWithoutTheQuotaAndByPriorityWithIt(t *testing.T) {
	facts := []modelFact{
		{Text: "first", Priority: PriorityLow},
		{Text: "second"},
		{Text: "third", Priority: PriorityHigh},
		{Text: "fourth", Priority: PriorityLow},
	}
	positional := truncateFacts(facts, 2, false)
	if len(positional) != 2 || positional[0].Text != "first" || positional[1].Text != "second" {
		t.Fatalf("positional truncation = %#v", positional)
	}
	ranked := truncateFacts(facts, 2, true)
	// High first, then the undocumented middle rank in the model's own order.
	if len(ranked) != 2 || ranked[0].Text != "third" || ranked[1].Text != "second" {
		t.Fatalf("priority truncation = %#v", ranked)
	}
	// A batch under the cap is handed back untouched, and the ranking never
	// reorders the caller's slice in place.
	if got := truncateFacts(facts, 9, true); len(got) != len(facts) || got[0].Text != "first" {
		t.Fatalf("under-cap batch = %#v", got)
	}
	if facts[0].Text != "first" || facts[3].Text != "fourth" {
		t.Fatalf("truncateFacts reordered its input: %#v", facts)
	}
}

// TestPriorityRankPutsAnUndocumentedValueInTheMiddle keeps the quota honest for a
// provider that ignores the priority request: a batch without the field ranks as
// one group, so it is truncated by position exactly as it was before the quota
// existed, rather than being sorted behind every fact that carries the word
// "high".
func TestPriorityRankPutsAnUndocumentedValueInTheMiddle(t *testing.T) {
	for value, want := range map[string]int{
		"high": 2, " HIGH ": 2, "low": 0, "low\n": 0, "": 1, "medium": 1, "urgent": 1,
	} {
		if got := priorityRank(value); got != want {
			t.Errorf("priorityRank(%q) = %d, want %d", value, got, want)
		}
	}
}
