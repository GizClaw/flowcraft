package component

import "sort"

// MergeCandidates merges per-shard result lists into the top limit of their
// union, under the order the lanes publish: higher score, then lower id.
//
// A sharded lane reads its shards one at a time and keeps each one's best
// results; the union is what the lane would have produced from a single table
// holding them all, because the order is total. Both halves matter: two
// candidates with the same score are ordered by id, so a merge that kept
// arrival order would rank one shard's record above another's for no reason
// the scores give, and the same query would answer differently depending on
// which shard was read first.
//
// A limit of zero or less means no limit.
func MergeCandidates(lists [][]Candidate, limit int) []Candidate {
	total := 0
	for _, list := range lists {
		total += len(list)
	}
	if total == 0 {
		return []Candidate{}
	}
	merged := make([]Candidate, 0, total)
	for _, list := range lists {
		merged = append(merged, list...)
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Score != merged[j].Score {
			return merged[i].Score > merged[j].Score
		}
		return merged[i].ID < merged[j].ID
	})
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}
