package relation

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

func scope() model.Scope { return model.Scope{RuntimeID: "rt", UserID: "u1"} }

func agentScope(agentID string) model.Scope {
	s := scope()
	s.AgentID = agentID
	return s
}

func TestRelation_LookupByDimensions(t *testing.T) {
	p := New()
	ctx := context.Background()
	facts := []model.TemporalFact{
		{ID: "r1", Scope: scope(), Kind: model.KindRelation,
			Subject: "alice", Predicate: "spouse", Object: "bob",
			ObservedAt: time.Unix(1, 0)},
		{ID: "r2", Scope: scope(), Kind: model.KindRelation,
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
	if err := p.Project(ctx, []model.TemporalFact{{
		ID: "r1", Scope: scope(), Kind: model.KindRelation,
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
	f := model.TemporalFact{
		ID: "r1", Scope: scope(), Kind: model.KindRelation,
		Subject: "alice", Predicate: "city", Object: "nyc",
		ObservedAt: past, ValidTo: &past,
	}
	if err := p.Project(ctx, []model.TemporalFact{f}); err != nil {
		t.Fatal(err)
	}
	if got := p.Lookup(ctx, scope(), "alice", "city", "nyc"); len(got) != 0 {
		t.Fatalf("expired relation must not index, got %+v", got)
	}
}

func TestRelation_RebuildDropsSuperseded(t *testing.T) {
	p := New()
	ctx := context.Background()
	if err := p.Project(ctx, []model.TemporalFact{
		{ID: "old", Scope: scope(), Kind: model.KindRelation,
			Subject: "a", Predicate: "p", Object: "o",
			ObservedAt: time.Unix(1, 0), CorrectedBy: "new"},
		{ID: "new", Scope: scope(), Kind: model.KindRelation,
			Subject: "a", Predicate: "p", Object: "o2",
			ObservedAt: time.Unix(2, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Rebuild(ctx, scope(), []model.TemporalFact{
		{ID: "old", Scope: scope(), Kind: model.KindRelation,
			Subject: "a", Predicate: "p", Object: "o",
			ObservedAt: time.Unix(1, 0), CorrectedBy: "new"},
		{ID: "new", Scope: scope(), Kind: model.KindRelation,
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
	if err := p.Project(ctx, []model.TemporalFact{
		{ID: "agent-a", Scope: agentScope("agent-a"), Kind: model.KindRelation,
			Subject: "alice", Predicate: "spouse", Object: "bob",
			ObservedAt: time.Unix(1, 0)},
		{ID: "agent-b", Scope: agentScope("agent-b"), Kind: model.KindRelation,
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
	if err := p.Project(ctx, []model.TemporalFact{
		{ID: "old", Scope: scope(), Kind: model.KindRelation,
			Subject: "alice", Predicate: "spouse", Object: "bob",
			ObservedAt: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Project(ctx, []model.TemporalFact{
		{ID: "new", Scope: scope(), Kind: model.KindRelation,
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
