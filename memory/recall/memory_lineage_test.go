package recall

import (
	"context"
	"errors"
	"sort"
	"testing"

	temporalstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/temporal"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// TestLineage_SingleNodeReturnsRootOnly pins the trivial case: a
// fact with no revision metadata yields a one-node DAG at depth 0
// classified as root.
func TestLineage_SingleNodeReturnsRootOnly(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "lone"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rootID := res.FactIDs[0]
	nodes, err := mem.Lineage(context.Background(), scope, rootID)
	if err != nil {
		t.Fatalf("Lineage: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("want 1 node, got %d (%+v)", len(nodes), nodes)
	}
	if nodes[0].Fact.ID != rootID || nodes[0].Depth != 0 || nodes[0].Relation != LineageRelationRoot {
		t.Errorf("root node mis-shaped: %+v", nodes[0])
	}
	if nodes[0].SourceFactID != "" {
		t.Errorf("root SourceFactID must be empty, got %q", nodes[0].SourceFactID)
	}
}

// TestLineage_FollowsSupersedeChain saves two FactState entries
// with the same merge key so the resolver closes the prior. Lineage
// from the prior must surface both, with the successor at depth 1
// relation=supersedes (reached via CorrectedBy).
func TestLineage_FollowsSupersedeChain(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	first, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city", Content: "Paris",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city", Content: "Berlin",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	priorID := first.FactIDs[0]
	succID := second.FactIDs[0]

	nodes, err := mem.Lineage(context.Background(), scope, priorID)
	if err != nil {
		t.Fatalf("Lineage: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("want 2 nodes, got %d (%+v)", len(nodes), nodes)
	}
	if nodes[0].Fact.ID != priorID || nodes[0].Depth != 0 || nodes[0].Relation != LineageRelationRoot {
		t.Errorf("node 0 mis-shaped: %+v", nodes[0])
	}
	if nodes[1].Fact.ID != succID || nodes[1].Depth != 1 || nodes[1].Relation != LineageRelationSupersede {
		t.Errorf("node 1 mis-shaped: %+v want id=%q depth=1 supersedes", nodes[1], succID)
	}
}

// TestLineage_FollowsForkBranch pins the fork edge: a parallel
// revision via Memory.Fork must appear at depth 1 with
// Relation=fork_of and SourceFactID = root.
func TestLineage_FollowsForkBranch(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	first, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city", Content: "Paris",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rootID := first.FactIDs[0]
	forked, err := mem.Fork(context.Background(), scope, rootID, TemporalFact{
		Kind: FactState, Subject: "alice", Predicate: "city", Content: "Lyon",
	})
	if err != nil {
		t.Fatal(err)
	}
	forkID := forked.FactIDs[0]

	nodes, err := mem.Lineage(context.Background(), scope, rootID)
	if err != nil {
		t.Fatalf("Lineage: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("want 2 nodes, got %d (%+v)", len(nodes), nodes)
	}
	if nodes[0].Fact.ID != rootID || nodes[0].Relation != LineageRelationRoot {
		t.Errorf("root node mis-shaped: %+v", nodes[0])
	}
	if nodes[1].Fact.ID != forkID || nodes[1].Depth != 1 || nodes[1].Relation != LineageRelationFork {
		t.Errorf("fork node mis-shaped: %+v want id=%q depth=1 fork_of", nodes[1], forkID)
	}
	if nodes[1].SourceFactID != rootID {
		t.Errorf("fork SourceFactID = %q, want %q", nodes[1].SourceFactID, rootID)
	}
}

// TestLineage_FollowsContestNote pins the contest edge: a
// challenge note produced by Memory.Contest must appear at depth 1
// with Relation=contest_of and SourceFactID = root.
func TestLineage_FollowsContestNote(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	first, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "claim under dispute"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rootID := first.FactIDs[0]
	contested, err := mem.Contest(context.Background(), scope, rootID, []EvidenceRef{{
		ID: "ev-1", Text: "I disagree",
	}})
	if err != nil {
		t.Fatal(err)
	}
	noteID := contested.FactIDs[0]

	nodes, err := mem.Lineage(context.Background(), scope, rootID)
	if err != nil {
		t.Fatalf("Lineage: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("want 2 nodes, got %d (%+v)", len(nodes), nodes)
	}
	if nodes[1].Fact.ID != noteID || nodes[1].Depth != 1 || nodes[1].Relation != LineageRelationContest {
		t.Errorf("contest node mis-shaped: %+v want id=%q depth=1 contest_of", nodes[1], noteID)
	}
	if nodes[1].SourceFactID != rootID {
		t.Errorf("contest SourceFactID = %q, want %q", nodes[1].SourceFactID, rootID)
	}
}

// TestLineage_DAGSortStable pins the within-depth (Depth asc, FactID
// asc) sort contract for a root with two parallel forks: regardless
// of insertion order the public output must be sorted by FactID.
func TestLineage_DAGSortStable(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	first, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city", Content: "Paris",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rootID := first.FactIDs[0]
	f1, err := mem.Fork(context.Background(), scope, rootID, TemporalFact{
		Kind: FactState, Subject: "alice", Predicate: "city", Content: "Lyon",
	})
	if err != nil {
		t.Fatal(err)
	}
	f2, err := mem.Fork(context.Background(), scope, rootID, TemporalFact{
		Kind: FactState, Subject: "alice", Predicate: "city", Content: "Nice",
	})
	if err != nil {
		t.Fatal(err)
	}
	forkIDs := []string{f1.FactIDs[0], f2.FactIDs[0]}

	nodes, err := mem.Lineage(context.Background(), scope, rootID)
	if err != nil {
		t.Fatalf("Lineage: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("want 3 nodes (root + 2 forks), got %d (%+v)", len(nodes), nodes)
	}
	if nodes[0].Fact.ID != rootID || nodes[0].Depth != 0 {
		t.Errorf("node 0 mis-shaped: %+v", nodes[0])
	}
	// nodes[1] and nodes[2] must be depth-1 forks sorted by FactID.
	wantForks := append([]string(nil), forkIDs...)
	if wantForks[0] > wantForks[1] {
		wantForks[0], wantForks[1] = wantForks[1], wantForks[0]
	}
	for i, want := range wantForks {
		got := nodes[i+1]
		if got.Fact.ID != want || got.Depth != 1 || got.Relation != LineageRelationFork {
			t.Errorf("node %d = %+v, want id=%q depth=1 fork_of", i+1, got, want)
		}
	}
	if !sort.SliceIsSorted(nodes[1:], func(i, j int) bool { return nodes[1:][i].Fact.ID < nodes[1:][j].Fact.ID }) {
		t.Errorf("depth-1 nodes not sorted by FactID: %+v", nodes[1:])
	}
}

// TestLineage_NonexistentFactReturnsNotFound pins the contract that
// a missing root surfaces as errdefs.NotFound (the underlying
// temporal store's ErrNotFound is classified as NotFound).
func TestLineage_NonexistentFactReturnsNotFound(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	_, err = mem.Lineage(context.Background(), scope, "ghost")
	if err == nil {
		t.Fatal("want error for missing fact")
	}
	if !errdefs.IsNotFound(err) {
		t.Errorf("err = %v, want NotFound-kind", err)
	}
	if !errors.Is(err, temporalstore.ErrNotFound) {
		t.Errorf("err = %v, want errors.Is(err, temporalstore.ErrNotFound)", err)
	}
}

// TestLineage_EmptyScopeReturnsValidationError pins the runtime_id
// guard so callers cannot accidentally walk the wrong tenant.
func TestLineage_EmptyScopeReturnsValidationError(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	_, err = mem.Lineage(context.Background(), Scope{}, "any")
	if err == nil {
		t.Fatal("want error for empty scope")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("err = %v, want Validation-kind", err)
	}
}

// TestLineage_EmptyFactIDReturnsValidationError pins the factID
// guard mirroring History / GetEvidence.
func TestLineage_EmptyFactIDReturnsValidationError(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	_, err = mem.Lineage(context.Background(), Scope{RuntimeID: "rt", UserID: "u1"}, "")
	if err == nil {
		t.Fatal("want error for empty factID")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("err = %v, want Validation-kind", err)
	}
}
