package graph

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func scope() domain.Scope { return domain.Scope{RuntimeID: "rt", UserID: "u1"} }

func TestGraph_TypedRelationEdgeTraverse(t *testing.T) {
	p := New()
	ctx := context.Background()
	if err := p.Project(ctx, []domain.TemporalFact{
		{ID: "r1", Scope: scope(), Kind: domain.KindRelation,
			Subject: "alice", Predicate: "friend", Object: "bob",
			ObservedAt: time.Unix(1, 0)},
		{ID: "r2", Scope: scope(), Kind: domain.KindRelation,
			Subject: "bob", Predicate: "friend", Object: "charlie",
			ObservedAt: time.Unix(2, 0)},
	}); err != nil {
		t.Fatal(err)
	}

	got := p.Traverse(ctx, scope(), []string{"alice"}, 2, 0)
	if !hasID(got, "r1") || !hasID(got, "r2") {
		t.Fatalf("2-hop traverse want r1+r2, got %+v", got)
	}
}

func TestGraph_SkipsCommonNounEndpoints(t *testing.T) {
	p := New()
	ctx := context.Background()
	if err := p.Project(ctx, []domain.TemporalFact{
		{ID: "r1", Scope: scope(), Kind: domain.KindRelation,
			Subject: "user", Predicate: "knows", Object: "alice",
			ObservedAt: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	if got := p.Traverse(ctx, scope(), []string{"alice"}, 1, 0); len(got) != 0 {
		t.Fatalf("common noun edge must not be indexed, got %+v", got)
	}
}

func TestGraph_CooccurrenceBounded(t *testing.T) {
	cfg := Config{MaxCooccurrenceParticipants: 2, MaxEdgesPerFact: 2}
	p := New(cfg)
	ctx := context.Background()
	if err := p.Project(ctx, []domain.TemporalFact{
		{ID: "e1", Scope: scope(), Kind: domain.KindEvent,
			Entities:   []string{"a", "b", "c"},
			ObservedAt: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	// only a-b pair fits participant cap of 2
	got := p.Traverse(ctx, scope(), []string{"a"}, 1, 0)
	if len(got) != 1 || got[0] != "e1" {
		t.Fatalf("bounded co-occurrence, got %+v", got)
	}
}

func TestGraph_IndexesProcedureCooccurrence(t *testing.T) {
	p := New()
	ctx := context.Background()
	if err := p.Project(ctx, []domain.TemporalFact{
		{ID: "proc1", Scope: scope(), Kind: domain.KindProcedure,
			Entities:   []string{"invoice", "ocr"},
			ObservedAt: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	got := p.Traverse(ctx, scope(), []string{"invoice"}, 1, 0)
	if len(got) != 1 || got[0] != "proc1" {
		t.Fatalf("procedure co-occurrence edge not indexed, got %+v", got)
	}
}

func TestGraph_ForgetRemovesEdges(t *testing.T) {
	p := New()
	ctx := context.Background()
	if err := p.Project(ctx, []domain.TemporalFact{
		{ID: "r1", Scope: scope(), Kind: domain.KindRelation,
			Subject: "alice", Predicate: "knows", Object: "bob",
			ObservedAt: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Forget(ctx, scope(), []string{"r1"}); err != nil {
		t.Fatal(err)
	}
	if got := p.Traverse(ctx, scope(), []string{"alice"}, 1, 0); len(got) != 0 {
		t.Fatalf("forget must drop edges, got %+v", got)
	}
}

// TestGraph_DropsClosed pins Cluster B: soft-forgotten (Closed)
// facts must not contribute edges to the graph projection. Before
// the predicate split, edges.go gated only on IsSuperseded for
// cooccurrence kinds and on IsActive (canonical) for relations, so
// Closed facts kept producing edges until RebuildAll.
func TestGraph_DropsClosed(t *testing.T) {
	p := New()
	ctx := context.Background()
	if err := p.Project(ctx, []domain.TemporalFact{
		{ID: "r1", Scope: scope(), Kind: domain.KindRelation,
			Subject: "alice", Predicate: "knows", Object: "bob",
			ObservedAt: time.Now(), Closed: true},
		{ID: "e1", Scope: scope(), Kind: domain.KindEvent,
			Entities:   []string{"alice", "bob"},
			ObservedAt: time.Now(), Closed: true},
	}); err != nil {
		t.Fatal(err)
	}
	if got := p.Traverse(ctx, scope(), []string{"alice"}, 2, 0); len(got) != 0 {
		t.Fatalf("Closed facts must not produce edges, got %+v", got)
	}
}

func TestGraph_RebuildExactReplace(t *testing.T) {
	p := New()
	ctx := context.Background()
	if err := p.Project(ctx, []domain.TemporalFact{
		{ID: "stale", Scope: scope(), Kind: domain.KindRelation,
			Subject: "alice", Predicate: "knows", Object: "bob",
			ObservedAt: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Rebuild(ctx, scope(), []domain.TemporalFact{
		{ID: "fresh", Scope: scope(), Kind: domain.KindRelation,
			Subject: "alice", Predicate: "knows", Object: "carol",
			ObservedAt: time.Unix(2, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	got := p.Traverse(ctx, scope(), []string{"alice"}, 1, 0)
	if len(got) != 1 || got[0] != "fresh" {
		t.Fatalf("rebuild exact replace failed: %+v", got)
	}
}

func TestGraph_AgentSoftIsolationBlocksPrivateBridge(t *testing.T) {
	p := New()
	ctx := context.Background()
	agentB := domain.Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-b"}
	shared := scope()
	agentA := domain.Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-a"}

	if err := p.Project(ctx, []domain.TemporalFact{
		{ID: "bridge", Scope: agentB, Kind: domain.KindRelation,
			Subject: "alice", Predicate: "knows", Object: "bob",
			ObservedAt: time.Unix(1, 0)},
		{ID: "shared", Scope: shared, Kind: domain.KindRelation,
			Subject: "bob", Predicate: "knows", Object: "carol",
			ObservedAt: time.Unix(2, 0)},
	}); err != nil {
		t.Fatal(err)
	}

	got := p.Traverse(ctx, agentA, []string{"alice"}, 2, 0)
	if hasID(got, "shared") {
		t.Fatalf("agent-a must not reach shared facts via agent-b private bridge, got %+v", got)
	}
}

func hasID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
