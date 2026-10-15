package vector

import (
	"context"
	"encoding/json"
	"maps"
	"math"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// normTable builds a table large enough to hold ties and filters: twelve
// entries drawing on four vectors, half of them in another conversation and a
// third of them summaries, so scores repeat and the order has to fall through
// to the id.
func normTable(t *testing.T) snapshot {
	t.Helper()
	vectors := []float32{
		1, 0, 0,
		0.5, 0.5, 0,
		0, 1, 0,
		0.25, 0.5, 0.75,
	}
	build := snapshot{Dimensions: 3}
	for index := range 12 {
		kind := corememory.ContextFact
		conversation := "conversation-a"
		name := "fact"
		switch {
		case index%3 == 0:
			name = "summary"
		case index%2 == 0:
			conversation = "conversation-b"
		}
		build.Entries = append(build.Entries, entry{
			ID:     entryIDs[index],
			Name:   name,
			Vector: append([]float32(nil), vectors[(index%4)*3:(index%4+1)*3]...),
			Source: corememory.SourceRef{Kind: corememory.SourceMessage, ID: "source"},
			Address: component.CandidateAddress{
				Kind: kind, ConversationID: conversation, ItemID: entryIDs[index],
			},
			ContentDigest: entryIDs[index],
		})
	}
	return build
}

// sameCandidate compares two candidates field by field: the score explanation
// carries a slice, so the values are not comparable with ==.
func sameCandidate(left, right component.Candidate) bool {
	if len(left.Explanation.Terms) != len(right.Explanation.Terms) {
		return false
	}
	for index := range left.Explanation.Terms {
		if left.Explanation.Terms[index] != right.Explanation.Terms[index] {
			return false
		}
	}
	left.Explanation, right.Explanation = component.ScoreExplanation{}, component.ScoreExplanation{}
	return left.Score == right.Score && left.ID == right.ID && left.Lane == right.Lane &&
		left.Name == right.Name && left.Source == right.Source && left.Address == right.Address &&
		maps.Equal(left.Metadata, right.Metadata)
}

var entryIDs = []string{
	"a-0", "b-1", "c-2", "d-3", "e-4", "f-5",
	"g-6", "h-7", "i-8", "j-9", "k-10", "l-11",
}

// TestStoredNormsMatchARecomputedScan pins the optimization to the behavior it
// replaced: the same table scored with the norms a read fills in and scored
// with every norm recomputed as before returns the same candidates, in the same
// order, over every filter and limit a caller uses. The reference is a table
// carrying no norms -- the shape a writer builds in memory -- so the lane's own
// fallback is what the fast path is compared against.
func TestStoredNormsMatchARecomputedScan(t *testing.T) {
	stored := normTable(t)
	if err := fillNorms(&stored); err != nil {
		t.Fatal(err)
	}
	recomputed := normTable(t)
	query := []float32{0.6, 0.8, 0}
	cases := []struct {
		name   string
		filter component.VectorSearchFilter
		limit  int
	}{
		{name: "no filter, no limit"},
		{name: "no filter, limit one", limit: 1},
		{name: "no filter, limit three", limit: 3},
		{name: "no filter, limit equal to the table", limit: 12},
		{name: "no filter, limit above the table", limit: 50},
		{name: "kind filter", filter: component.VectorSearchFilter{Name: "summary"}},
		{name: "kind filter with limit", filter: component.VectorSearchFilter{Name: "fact"}, limit: 2},
		{name: "conversation filter", filter: component.VectorSearchFilter{
			Metadata: corememory.Metadata{"conversation_id": "conversation-b"},
		}},
		{name: "conversation filter with limit", filter: component.VectorSearchFilter{
			Metadata: corememory.Metadata{"conversation_id": "conversation-a"},
		}, limit: 4},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			want, wantReport, err := searchSnapshot(recomputed, query, testCase.filter, testCase.limit)
			if err != nil {
				t.Fatal(err)
			}
			got, gotReport, err := searchSnapshot(stored, query, testCase.filter, testCase.limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(want) == 0 {
				t.Fatal("the fixture returned nothing")
			}
			if len(got) != len(want) {
				t.Fatalf("results = %d candidates, want %d", len(got), len(want))
			}
			for index := range want {
				if !sameCandidate(got[index], want[index]) {
					t.Fatalf("candidate %d = %+v, want %+v", index, got[index], want[index])
				}
			}
			// Both scans walk the same table and admit the same entries: the
			// stored norms change what scoring costs, not what it sees.
			if gotReport.scanned != wantReport.scanned || gotReport.scored != wantReport.scored {
				t.Fatalf("scanned/scored = %d/%d, want %d/%d",
					gotReport.scanned, gotReport.scored, wantReport.scanned, wantReport.scored)
			}
		})
	}
}

// TestStoredNormsAreDerivedFromTheVectors pins what a read fills in: one norm
// per entry, in the table's order, equal to the entry's own.
func TestStoredNormsAreDerivedFromTheVectors(t *testing.T) {
	runtime, model := fakeRuntime(t)
	index, err := New(Config{KV: kvFor(t), Runtime: runtime, Model: model, Projection: "facts"})
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	if err := index.Rebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Artifacts: []component.Artifact{vectorArtifact("a", "alpha"), vectorArtifact("b", "beta")},
	}); err != nil {
		t.Fatal(err)
	}
	build, err := index.readSnapshot(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(build.Entries) != 2 || len(build.norms) != len(build.Entries) {
		t.Fatalf("entries %d, norms %d", len(build.Entries), len(build.norms))
	}
	for position, item := range build.Entries {
		want, err := norm(item.Vector)
		if err != nil {
			t.Fatal(err)
		}
		if build.norms[position] != want {
			t.Fatalf("norm %d = %v, want %v", position, build.norms[position], want)
		}
	}
}

// TestStoredNormsAreNotPersisted pins the other half: the norms are derived
// from the entries, so they stay off the wire -- a table that carried them
// would change the digest of every generation it is written into.
func TestStoredNormsAreNotPersisted(t *testing.T) {
	build := normTable(t)
	if err := fillNorms(&build); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(build)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys["dimensions"] == nil || keys["entries"] == nil {
		t.Fatalf("persisted keys = %v, want dimensions and entries", keys)
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(keys["entries"], &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(build.Entries) {
		t.Fatalf("persisted entries = %d, want %d", len(entries), len(build.Entries))
	}
	for _, item := range entries {
		if len(item) != 6 {
			t.Fatalf("an entry carries %d fields, want the six it always had: %v", len(item), item)
		}
		for _, field := range []string{"id", "name", "vector", "source", "address", "content_digest"} {
			if item[field] == nil {
				t.Fatalf("entry field %q is missing: the persisted shape changed", field)
			}
		}
	}
	// The same bytes a table without norms encodes to.
	plain, err := json.Marshal(normTable(t))
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != string(plain) {
		t.Fatal("a table with norms does not encode to the bytes of one without")
	}
}

// TestFillNormsRejectsAnUnscorableVector pins the failure the scan used to
// report per query: a vector with no norm is rejected while the table is read,
// with the entry named, instead of once per query that would have admitted it.
func TestFillNormsRejectsAnUnscorableVector(t *testing.T) {
	build := normTable(t)
	build.Entries[0].Vector = []float32{0, 0, 0}
	err := fillNorms(&build)
	if err == nil {
		t.Fatal("a zero vector was accepted")
	}
	if !strings.Contains(err.Error(), build.Entries[0].ID) {
		t.Fatalf("the failure does not name the entry: %v", err)
	}
	build = normTable(t)
	build.Entries[1].Vector = []float32{float32(math.NaN()), 0, 0}
	if err := fillNorms(&build); err == nil {
		t.Fatal("a non-finite vector was accepted")
	}
}

// TestTableWithoutNormsIsScoredAnyway pins the fallback: a table in memory
// carries no norms until a read fills them, and a scan must still score it.
func TestTableWithoutNormsIsScoredAnyway(t *testing.T) {
	build := normTable(t)
	results, report, err := searchSnapshot(build, []float32{1, 0, 0}, component.VectorSearchFilter{}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || report.scored != len(build.Entries) {
		t.Fatalf("results %d, scored %d", len(results), report.scored)
	}
	if results[0].ID != entryIDs[0] || results[1].ID != entryIDs[4] || results[2].ID != entryIDs[8] {
		t.Fatalf("order = %v", candidateIDs(results))
	}
}
