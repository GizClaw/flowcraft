package domain

import (
	"context"
	"errors"
	"testing"
)

func lineageScope() Scope { return Scope{RuntimeID: "rt", UserID: "u1"} }

func lineageFact(id string) TemporalFact {
	return TemporalFact{ID: id, Scope: lineageScope(), Kind: KindNote, Content: "c-" + id}
}

// lookupsFromFacts builds an in-memory LineageLookups against a fixed
// set of facts so each test can declare just the topology.
func lookupsFromFacts(facts ...TemporalFact) (LineageLookups, map[string]TemporalFact) {
	byID := make(map[string]TemporalFact, len(facts))
	for _, f := range facts {
		byID[f.ID] = f
	}
	return LineageLookups{
		Get: func(_ context.Context, _ Scope, id string) (TemporalFact, error) {
			f, ok := byID[id]
			if !ok {
				return TemporalFact{}, errors.New("not found: " + id)
			}
			return f, nil
		},
		FindByRevisionSource: func(_ context.Context, _ Scope, src string) ([]TemporalFact, error) {
			var out []TemporalFact
			for _, f := range facts {
				rev, ok := RevisionOf(f)
				if !ok {
					continue
				}
				if rev.SourceFactID == src {
					out = append(out, f)
				}
			}
			return out, nil
		},
	}, byID
}

func TestBuildLineage_SingleNode(t *testing.T) {
	root := lineageFact("a")
	lookups, _ := lookupsFromFacts(root)
	got, err := BuildLineage(context.Background(), root, lookups)
	if err != nil {
		t.Fatalf("BuildLineage: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 node, got %d (%+v)", len(got), got)
	}
	if got[0].Fact.ID != "a" || got[0].Depth != 0 || got[0].Relation != LineageRelationRoot {
		t.Errorf("root node mis-shaped: %+v", got[0])
	}
	if got[0].SourceFactID != "" {
		t.Errorf("root SourceFactID must be empty, got %q", got[0].SourceFactID)
	}
}

func TestBuildLineage_SupersedeChain(t *testing.T) {
	a := lineageFact("a")
	a.CorrectedBy = "b"
	b := lineageFact("b")
	b.CorrectedBy = "c"
	b.Supersedes = []string{"a"}
	c := lineageFact("c")
	c.Supersedes = []string{"b"}

	lookups, _ := lookupsFromFacts(a, b, c)
	got, err := BuildLineage(context.Background(), a, lookups)
	if err != nil {
		t.Fatalf("BuildLineage: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 nodes for A→B→C chain, got %d (%+v)", len(got), got)
	}
	wantIDs := []string{"a", "b", "c"}
	wantDepths := []int{0, 1, 2}
	for i, n := range got {
		if n.Fact.ID != wantIDs[i] {
			t.Errorf("node %d id = %q, want %q", i, n.Fact.ID, wantIDs[i])
		}
		if n.Depth != wantDepths[i] {
			t.Errorf("node %d depth = %d, want %d", i, n.Depth, wantDepths[i])
		}
	}
	if got[0].Relation != LineageRelationRoot {
		t.Errorf("node 0 relation = %q, want root", got[0].Relation)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Relation != LineageRelationSupersede {
			t.Errorf("node %d relation = %q, want supersedes", i, got[i].Relation)
		}
	}
}

func TestBuildLineage_ForkBranch(t *testing.T) {
	a := lineageFact("a")
	b := lineageFact("b")
	AttachRevision(&b, Revision{Kind: RevisionFork, SourceFactID: "a"})

	lookups, _ := lookupsFromFacts(a, b)
	got, err := BuildLineage(context.Background(), a, lookups)
	if err != nil {
		t.Fatalf("BuildLineage: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 nodes, got %d (%+v)", len(got), got)
	}
	if got[0].Fact.ID != "a" || got[0].Relation != LineageRelationRoot || got[0].Depth != 0 {
		t.Errorf("root node mis-shaped: %+v", got[0])
	}
	if got[1].Fact.ID != "b" || got[1].Relation != LineageRelationFork || got[1].Depth != 1 {
		t.Errorf("fork node mis-shaped: %+v", got[1])
	}
	if got[1].SourceFactID != "a" {
		t.Errorf("fork SourceFactID = %q, want a", got[1].SourceFactID)
	}
}

func TestBuildLineage_ContestNote(t *testing.T) {
	a := lineageFact("a")
	b := lineageFact("b")
	AttachRevision(&b, Revision{Kind: RevisionContest, SourceFactID: "a"})

	lookups, _ := lookupsFromFacts(a, b)
	got, err := BuildLineage(context.Background(), a, lookups)
	if err != nil {
		t.Fatalf("BuildLineage: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 nodes, got %d (%+v)", len(got), got)
	}
	if got[1].Fact.ID != "b" || got[1].Relation != LineageRelationContest || got[1].Depth != 1 {
		t.Errorf("contest node mis-shaped: %+v", got[1])
	}
	if got[1].SourceFactID != "a" {
		t.Errorf("contest SourceFactID = %q, want a", got[1].SourceFactID)
	}
}

// TestBuildLineage_DAG_Deterministic pins the within-depth sort
// contract: three forks of the same root must appear sorted by
// FactID regardless of insertion order or lookup return order.
func TestBuildLineage_DAG_Deterministic(t *testing.T) {
	a := lineageFact("a")
	fc := lineageFact("c-fork")
	AttachRevision(&fc, Revision{Kind: RevisionFork, SourceFactID: "a"})
	fa := lineageFact("a-fork")
	AttachRevision(&fa, Revision{Kind: RevisionFork, SourceFactID: "a"})
	fb := lineageFact("b-fork")
	AttachRevision(&fb, Revision{Kind: RevisionFork, SourceFactID: "a"})

	// Pass deliberately out of order so a stable BFS without the
	// final sort would emit c-fork, a-fork, b-fork.
	lookups, _ := lookupsFromFacts(a, fc, fa, fb)
	got, err := BuildLineage(context.Background(), a, lookups)
	if err != nil {
		t.Fatalf("BuildLineage: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 nodes, got %d (%+v)", len(got), got)
	}
	wantOrder := []string{"a", "a-fork", "b-fork", "c-fork"}
	for i, want := range wantOrder {
		if got[i].Fact.ID != want {
			t.Errorf("node %d id = %q, want %q (full order: %v)", i, got[i].Fact.ID, want, idsOf(got))
		}
	}
	if got[0].Depth != 0 {
		t.Errorf("root depth = %d, want 0", got[0].Depth)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Depth != 1 {
			t.Errorf("fork node %d depth = %d, want 1", i, got[i].Depth)
		}
		if got[i].Relation != LineageRelationFork {
			t.Errorf("fork node %d relation = %q, want fork_of", i, got[i].Relation)
		}
	}
}

// TestBuildLineage_CycleSafe pins traversal termination on
// deliberately malformed circular metadata where A.Supersedes
// points at B and B.Supersedes points back at A.
func TestBuildLineage_CycleSafe(t *testing.T) {
	a := lineageFact("a")
	a.Supersedes = []string{"b"}
	b := lineageFact("b")
	b.Supersedes = []string{"a"}

	lookups, _ := lookupsFromFacts(a, b)
	got, err := BuildLineage(context.Background(), a, lookups)
	if err != nil {
		t.Fatalf("BuildLineage: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want exactly 2 nodes (A,B) on cycle, got %d (%+v)", len(got), got)
	}
	if got[0].Fact.ID != "a" || got[1].Fact.ID != "b" {
		t.Errorf("cycle order = %v, want [a b]", idsOf(got))
	}
}

func idsOf(nodes []FactLineageNode) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Fact.ID
	}
	return out
}
