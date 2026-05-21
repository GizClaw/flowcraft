package relation

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

func scope() domain.Scope { return domain.Scope{RuntimeID: "rt", UserID: "u1"} }

func agentScope(agentID string) domain.Scope {
	s := scope()
	s.AgentID = agentID
	return s
}

func TestRelation_LookupByDimensions(t *testing.T) {
	p := New()
	ctx := context.Background()
	facts := []domain.TemporalFact{
		{ID: "r1", Scope: scope(), Kind: domain.KindRelation,
			Subject: "alice", Predicate: "spouse", Object: "bob",
			ObservedAt: time.Unix(1, 0)},
		{ID: "r2", Scope: scope(), Kind: domain.KindRelation,
			Subject: "alice", Predicate: "spouse", Object: "carol",
			ObservedAt: time.Unix(2, 0)},
	}
	if err := p.Project(ctx, facts); err != nil {
		t.Fatal(err)
	}
	if got := p.Lookup(ctx, scope(), "alice", "spouse", "bob"); len(got) != 1 || got[0] != "r1" {
		t.Errorf("triple lookup = %+v", got)
	}
	if got := p.Lookup(ctx, scope(), "alice", "", ""); len(got) != 2 {
		t.Errorf("subject-only lookup = %+v", got)
	}
}

func TestRelation_LookupCanonicalizesDimensions(t *testing.T) {
	p := New()
	ctx := context.Background()
	if err := p.Project(ctx, []domain.TemporalFact{{
		ID: "r1", Scope: scope(), Kind: domain.KindRelation,
		Subject: "Alice", Predicate: "Spouse", Object: "Bob",
		ObservedAt: time.Unix(1, 0),
	}}); err != nil {
		t.Fatal(err)
	}

	if got := p.Lookup(ctx, scope(), "alice", "spouse", "bob"); len(got) != 1 || got[0] != "r1" {
		t.Fatalf("lookup should be case-insensitive across structured dimensions, got %+v", got)
	}
	if got := p.Lookup(ctx, scope(), " ALICE ", "", ""); len(got) != 1 || got[0] != "r1" {
		t.Fatalf("subject lookup should trim and canonicalize, got %+v", got)
	}
}

func TestRelation_DropsInactiveValidTo(t *testing.T) {
	p := New()
	ctx := context.Background()
	past := time.Unix(1, 0)
	f := domain.TemporalFact{
		ID: "r1", Scope: scope(), Kind: domain.KindRelation,
		Subject: "alice", Predicate: "city", Object: "nyc",
		ObservedAt: past, ValidTo: &past,
	}
	if err := p.Project(ctx, []domain.TemporalFact{f}); err != nil {
		t.Fatal(err)
	}
	if got := p.Lookup(ctx, scope(), "alice", "city", "nyc"); len(got) != 0 {
		t.Fatalf("expired relation must not index, got %+v", got)
	}
}

// TestRelation_DropsClosed pins Cluster B: soft-forgotten (Closed)
// relations must not survive in the projection cache. Before the
// predicate split, the projection called IsActive (canonical only)
// and re-upserted closed triples after a soft Forget.
func TestRelation_DropsClosed(t *testing.T) {
	p := New()
	ctx := context.Background()
	f := domain.TemporalFact{
		ID: "r1", Scope: scope(), Kind: domain.KindRelation,
		Subject: "alice", Predicate: "spouse", Object: "bob",
		ObservedAt: time.Now(), Closed: true,
	}
	if err := p.Project(ctx, []domain.TemporalFact{f}); err != nil {
		t.Fatal(err)
	}
	if got := p.Lookup(ctx, scope(), "alice", "spouse", "bob"); len(got) != 0 {
		t.Fatalf("Closed relation must not appear in lookup, got %+v", got)
	}
}

func TestRelation_RebuildDropsSuperseded(t *testing.T) {
	p := New()
	ctx := context.Background()
	if err := p.Project(ctx, []domain.TemporalFact{
		{ID: "old", Scope: scope(), Kind: domain.KindRelation,
			Subject: "a", Predicate: "p", Object: "o",
			ObservedAt: time.Unix(1, 0), CorrectedBy: "new"},
		{ID: "new", Scope: scope(), Kind: domain.KindRelation,
			Subject: "a", Predicate: "p", Object: "o2",
			ObservedAt: time.Unix(2, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Rebuild(ctx, scope(), []domain.TemporalFact{
		{ID: "old", Scope: scope(), Kind: domain.KindRelation,
			Subject: "a", Predicate: "p", Object: "o",
			ObservedAt: time.Unix(1, 0), CorrectedBy: "new"},
		{ID: "new", Scope: scope(), Kind: domain.KindRelation,
			Subject: "a", Predicate: "p", Object: "o2",
			ObservedAt: time.Unix(2, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	if got := p.Lookup(ctx, scope(), "a", "p", "o"); len(got) != 0 {
		t.Errorf("superseded relation must not reappear after rebuild")
	}
	if got := p.Lookup(ctx, scope(), "a", "p", "o2"); len(got) != 1 {
		t.Errorf("active relation missing after rebuild: %+v", got)
	}
}

func TestRelation_PreservesAgentPrivateFactsForSameTriple(t *testing.T) {
	p := New()
	ctx := context.Background()
	if err := p.Project(ctx, []domain.TemporalFact{
		{ID: "agent-a", Scope: agentScope("agent-a"), Kind: domain.KindRelation,
			Subject: "alice", Predicate: "spouse", Object: "bob",
			ObservedAt: time.Unix(1, 0)},
		{ID: "agent-b", Scope: agentScope("agent-b"), Kind: domain.KindRelation,
			Subject: "alice", Predicate: "spouse", Object: "bob",
			ObservedAt: time.Unix(2, 0)},
	}); err != nil {
		t.Fatal(err)
	}

	got := p.Lookup(ctx, scope(), "alice", "spouse", "bob")
	if !hasID(got, "agent-a") || !hasID(got, "agent-b") {
		t.Fatalf("cross-agent relation query must not lose private facts sharing a triple, got %+v", got)
	}
}

func TestRelation_ForgetOldOverwrittenSlotKeepsCurrentFact(t *testing.T) {
	p := New()
	ctx := context.Background()
	if err := p.Project(ctx, []domain.TemporalFact{
		{ID: "old", Scope: scope(), Kind: domain.KindRelation,
			Subject: "alice", Predicate: "spouse", Object: "bob",
			ObservedAt: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Project(ctx, []domain.TemporalFact{
		{ID: "new", Scope: scope(), Kind: domain.KindRelation,
			Subject: "alice", Predicate: "spouse", Object: "bob",
			ObservedAt: time.Unix(2, 0)},
	}); err != nil {
		t.Fatal(err)
	}

	if err := p.Forget(ctx, scope(), []string{"old"}); err != nil {
		t.Fatal(err)
	}
	if got := p.Lookup(ctx, scope(), "alice", "spouse", "bob"); !hasID(got, "new") {
		t.Fatalf("forgetting old fact must not remove current relation slot, got %+v", got)
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
