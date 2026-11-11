package chat

import (
	"slices"
	"strings"
)

// The priorities the quota prompt asks the extractor for. Only the two ends are
// named: a value the prompt does not document, or a provider that dropped the
// field, ranks between them (see priorityRank).
const (
	// PriorityHigh marks a claim a question asked months later can be answered
	// from on its own.
	PriorityHigh = "high"
	// PriorityLow marks a claim that only carries its meaning beside another
	// fact of the same turn.
	PriorityLow = "low"
)

// truncateFacts bounds a batch at maxFacts. With byPriority set, the facts the
// extractor marked high come first, in the model's own order; otherwise the
// batch is cut at that order. Truncating instead of failing is the point: a
// chatty generation would otherwise fail the commit deterministically and replay
// forever.
//
// Which facts an over-full batch drops only matters where a host caps the batch
// deliberately -- the extractor's own cap is high enough that the batches this
// workspace's conversations produce never reach it -- but where it does matter
// the difference is the whole batch: by position the tail of the model's answer
// goes, whatever it says, and by priority the claims the extractor itself judged
// unable to stand alone go first.
func truncateFacts(facts []modelFact, maxFacts int, byPriority bool) []modelFact {
	if len(facts) <= maxFacts {
		return facts
	}
	if !byPriority {
		return facts[:maxFacts]
	}
	ranked := make([]modelFact, len(facts))
	copy(ranked, facts)
	slices.SortStableFunc(ranked, func(left, right modelFact) int {
		return priorityRank(right.Priority) - priorityRank(left.Priority)
	})
	return ranked[:maxFacts]
}

// priorityRank orders the extractor's self-assessment: a fact marked high is one
// a later question can be answered from on its own, a fact marked low only
// matters beside another fact, and anything else ranks in between. The middle
// rank is what keeps the quota honest for a provider that ignores the request:
// such a batch is truncated by position, as it always was, instead of being
// sorted behind every fact that happens to carry the word "high".
func priorityRank(priority string) int {
	switch strings.ToLower(strings.TrimSpace(priority)) {
	case PriorityHigh:
		return 2
	case PriorityLow:
		return 0
	default:
		return 1
	}
}
