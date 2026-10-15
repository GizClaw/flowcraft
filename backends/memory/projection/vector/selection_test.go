package vector

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	projectionstore "github.com/GizClaw/flowcraft/backends/memory/internal/projection"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// referenceScan is the scan as the lane published it before the bounded
// selection: it scores every entry the filter admits into a candidate, sorts
// all of them by score descending and id ascending, and then takes the limit.
// It shares no code with the scan, so comparing the two compares an
// implementation against the behavior it has to keep.
func referenceScan(t *testing.T, build snapshot, query []float32, filter component.VectorSearchFilter, limit int) []component.Candidate {
	t.Helper()
	queryNorm, err := norm(query)
	if err != nil {
		t.Fatal(err)
	}
	var results []component.Candidate
	for _, item := range build.Entries {
		if filter.Name != "" && item.Name != filter.Name {
			continue
		}
		matches, err := projectionstore.MatchesRequest(filter.Metadata, item.Address)
		if err != nil {
			t.Fatal(err)
		}
		if !matches {
			continue
		}
		itemNorm, err := norm(item.Vector)
		if err != nil {
			t.Fatal(err)
		}
		var dot float64
		for index := range query {
			dot += float64(query[index]) * float64(item.Vector[index])
		}
		cosine := max(-1, min(1, dot/(queryNorm*itemNorm)))
		results = append(results, component.Candidate{
			ID: item.ID, Lane: laneName, Name: item.Name, Score: cosine,
			Source: item.Source, Address: item.Address, Metadata: item.Metadata.Clone(),
		})
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].ID < results[j].ID
		}
		return results[i].Score > results[j].Score
	})
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results
}

// TestBoundedSelectionMatchesAFullSort is the equivalence test the
// optimization stands on: over tables that hold ties, every filter shape, and
// every limit a caller can ask for, the scan that keeps a bounded selection
// returns exactly what the scan that sorted every match returned.
func TestBoundedSelectionMatchesAFullSort(t *testing.T) {
	source := rand.New(rand.NewSource(11))
	build := quantizedSnapshot(t, source, 400, 16)
	if err := fillNorms(&build); err != nil {
		t.Fatal(err)
	}
	query := randomVector(source, build.Dimensions)
	shapes := []struct {
		name   string
		filter component.VectorSearchFilter
	}{
		{name: "everything"},
		{name: "kind", filter: component.VectorSearchFilter{Name: "fact"}},
		{name: "conversation", filter: component.VectorSearchFilter{
			Metadata: corememory.Metadata{"conversation_id": "conversation-b"},
		}},
		{name: "conversation and kind", filter: component.VectorSearchFilter{
			Name: "summary", Metadata: corememory.Metadata{"conversation_id": "conversation-a"},
		}},
	}
	for _, limit := range []int{0, 1, 2, 3, 7, 40, 399, 400, 401, 1000} {
		for _, shape := range shapes {
			want := referenceScan(t, build, query, shape.filter, limit)
			if len(want) == 0 {
				t.Fatal("the fixture admitted nothing")
			}
			got, report, err := searchSnapshot(build, query, shape.filter, limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("limit %d, %s: results = %d candidates, want %d", limit, shape.name, len(got), len(want))
			}
			for index := range want {
				if !sameCandidate(got[index], want[index]) {
					t.Fatalf("limit %d, %s: candidate %d = %+v, want %+v",
						limit, shape.name, index, got[index], want[index])
				}
			}
			if limit > 0 && len(got) > limit {
				t.Fatalf("limit %d, %s: %d candidates came back", limit, shape.name, len(got))
			}
			if report.scored < len(got) {
				t.Fatalf("limit %d, %s: scored %d for %d candidates", limit, shape.name, report.scored, len(got))
			}
		}
	}
}

// TestSelectionKeepsTheBestEntries pins the heap itself: whatever order
// entries arrive in, the selection holds the limit best of them, the worst one
// it holds sits at the root -- the entry a later offer would replace -- and
// the result comes back in the order the scan publishes.
func TestSelectionKeepsTheBestEntries(t *testing.T) {
	source := rand.New(rand.NewSource(3))
	for _, limit := range []int{1, 2, 5, 31} {
		var everything []ranked
		selection := newSelection(limit, 500)
		for index := range 500 {
			// Scores repeat on purpose: the id has to decide the ties.
			candidate := ranked{index: index, id: entryName(index), score: float64(source.Intn(20)) / 4}
			everything = append(everything, candidate)
			selection.offer(candidate)
			if len(selection.items) == limit && selection.items[0] != worstOf(selection.items) {
				t.Fatalf("limit %d: the root is not the entry the selection drops first", limit)
			}
		}
		want := append([]ranked(nil), everything...)
		sort.Slice(want, func(i, j int) bool { return want[j].ranksAfter(want[i]) })
		want = want[:limit]
		got := selection.result()
		if len(got) != len(want) {
			t.Fatalf("limit %d: kept %d entries, want %d", limit, len(got), len(want))
		}
		for index := range want {
			if got[index] != want[index] {
				t.Fatalf("limit %d: entry %d = %+v, want %+v", limit, index, got[index], want[index])
			}
		}
	}
}

// TestSelectionWithNoBoundKeepsEverything pins the unbounded path: a scan that
// asks for the whole table, or for more than the table holds, returns every
// match it admitted, ordered -- the shape the lane served before it had a
// bounded selection.
func TestSelectionWithNoBoundKeepsEverything(t *testing.T) {
	for _, limit := range []int{0, -1, 500, 501} {
		selection := newSelection(limit, 500)
		if selection.limit != 0 {
			t.Fatalf("limit %d: the selection bounds at %d", limit, selection.limit)
		}
		for index := range 500 {
			selection.offer(ranked{index: index, id: entryName(index), score: float64(index % 7)})
		}
		got := selection.result()
		if len(got) != 500 {
			t.Fatalf("limit %d: kept %d entries, want 500", limit, len(got))
		}
		for index := 1; index < len(got); index++ {
			if got[index-1].ranksAfter(got[index]) {
				t.Fatalf("limit %d: entry %d ranks after the one that follows it", limit, index-1)
			}
		}
	}
}

// worstOf returns the entry of a selection that ranks after every other: the
// one it drops first.
func worstOf(items []ranked) ranked {
	worst := items[0]
	for _, item := range items[1:] {
		if item.ranksAfter(worst) {
			worst = item
		}
	}
	return worst
}

func entryName(index int) string { return fmt.Sprintf("entry-%04d", index) }

// quantizedSnapshot builds a table whose vectors come from a small set of
// directions: scores repeat, so the ordering has to fall through to the id, and
// both paths have to break ties the same way.
func quantizedSnapshot(t *testing.T, source *rand.Rand, size, dimensions int) snapshot {
	t.Helper()
	build := snapshot{Dimensions: dimensions}
	directions := make([][]float32, 24)
	for index := range directions {
		directions[index] = randomVector(source, dimensions)
	}
	for index := range size {
		conversation := "conversation-a"
		if index%3 == 0 {
			conversation = "conversation-b"
		}
		name := "fact"
		if index%5 == 0 {
			name = "summary"
		}
		build.Entries = append(build.Entries, entry{
			ID:     entryName(index),
			Name:   name,
			Vector: append([]float32(nil), directions[index%len(directions)]...),
			Source: corememory.SourceRef{Kind: corememory.SourceMessage, ID: "source"},
			Address: component.CandidateAddress{
				Kind: corememory.ContextFact, ConversationID: conversation, ItemID: entryName(index),
			},
			ContentDigest: entryName(index),
		})
	}
	return build
}
