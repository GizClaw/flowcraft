package profile

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

func TestProfile_LookupBySubject(t *testing.T) {
	p := New()
	ctx := context.Background()
	facts := []domain.TemporalFact{
		{ID: "s1", Scope: scope(), Kind: domain.KindState,
			Subject: "alice", Predicate: "city", Content: "nyc",
			ObservedAt: time.Unix(1, 0)},
		{ID: "p1", Scope: scope(), Kind: domain.KindPreference,
			Subject: "alice", Predicate: "food", Content: "sushi",
			ObservedAt: time.Unix(2, 0)},
		{ID: "r1", Scope: scope(), Kind: domain.KindRelation,
			Subject: "alice", Predicate: "spouse", Object: "bob",
			ObservedAt: time.Unix(3, 0)},
	}
	if err := p.Project(ctx, facts); err != nil {
		t.Fatal(err)
	}
	got := p.Lookup(ctx, scope(), "alice")
	if len(got) != 3 {
		t.Fatalf("want 3 active slots for alice, got %+v", got)
	}
}

func TestProfile_LookupCanonicalizesSubject(t *testing.T) {
	p := New()
	ctx := context.Background()
	if err := p.Project(ctx, []domain.TemporalFact{{
		ID: "s1", Scope: scope(), Kind: domain.KindState,
		Subject: "Alice", Predicate: "City", Content: "Paris",
		ObservedAt: time.Unix(1, 0),
	}}); err != nil {
		t.Fatal(err)
	}

	if got := p.Lookup(ctx, scope(), "alice"); len(got) != 1 || got[0] != "s1" {
		t.Fatalf("lookup should be case-insensitive for subject, got %+v", got)
	}
	if got := p.Lookup(ctx, scope(), " ALICE "); len(got) != 1 || got[0] != "s1" {
		t.Fatalf("lookup should trim and canonicalize subject, got %+v", got)
	}
}

func TestProfile_DropsExpiredSlot(t *testing.T) {
	p := New()
	ctx := context.Background()
	past := time.Unix(1, 0)
	f := domain.TemporalFact{
		ID: "s1", Scope: scope(), Kind: domain.KindState,
		Subject: "alice", Predicate: "city", Content: "nyc",
		ObservedAt: past, ValidTo: &past,
	}
	if err := p.Project(ctx, []domain.TemporalFact{f}); err != nil {
		t.Fatal(err)
	}
	if got := p.Lookup(ctx, scope(), "alice"); len(got) != 0 {
		t.Fatalf("expired state must not be in profile, got %+v", got)
	}
}

// TestProfile_DropsClosed pins Cluster B: a soft-forgotten (Closed)
// fact must not survive in the profile projection cache. Before the
// predicate split, the projection called IsActive (canonical only)
// and would re-upsert a closed slot.
func TestProfile_DropsClosed(t *testing.T) {
	p := New()
	ctx := context.Background()
	f := domain.TemporalFact{
		ID: "s1", Scope: scope(), Kind: domain.KindState,
		Subject: "alice", Predicate: "city", Content: "nyc",
		ObservedAt: time.Now(), Closed: true,
	}
	if err := p.Project(ctx, []domain.TemporalFact{f}); err != nil {
		t.Fatal(err)
	}
	if got := p.Lookup(ctx, scope(), "alice"); len(got) != 0 {
		t.Fatalf("Closed fact must not appear in profile lookup, got %+v", got)
	}
}

func TestProfile_RebuildExactReplace(t *testing.T) {
	p := New()
	ctx := context.Background()
	if err := p.Project(ctx, []domain.TemporalFact{
		{ID: "stale", Scope: scope(), Kind: domain.KindState,
			Subject: "bob", Predicate: "city", ObservedAt: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Rebuild(ctx, scope(), []domain.TemporalFact{
		{ID: "fresh", Scope: scope(), Kind: domain.KindPreference,
			Subject: "bob", Predicate: "food", ObservedAt: time.Unix(2, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	got := p.Lookup(ctx, scope(), "bob")
	if len(got) != 1 || got[0] != "fresh" {
		t.Errorf("rebuild exact replace failed: %+v", got)
	}
}

func TestProfile_PreservesAgentPrivateFactsForSameSlot(t *testing.T) {
	p := New()
	ctx := context.Background()
	if err := p.Project(ctx, []domain.TemporalFact{
		{ID: "agent-a", Scope: agentScope("agent-a"), Kind: domain.KindState,
			Subject: "alice", Predicate: "city", Content: "Paris",
			ObservedAt: time.Unix(1, 0)},
		{ID: "agent-b", Scope: agentScope("agent-b"), Kind: domain.KindState,
			Subject: "alice", Predicate: "city", Content: "Berlin",
			ObservedAt: time.Unix(2, 0)},
	}); err != nil {
		t.Fatal(err)
	}

	got := p.Lookup(ctx, scope(), "alice")
	if !hasID(got, "agent-a") || !hasID(got, "agent-b") {
		t.Fatalf("cross-agent profile query must not lose private facts sharing a slot, got %+v", got)
	}
}

func TestProfile_ForgetOldOverwrittenSlotKeepsCurrentFact(t *testing.T) {
	p := New()
	ctx := context.Background()
	if err := p.Project(ctx, []domain.TemporalFact{
		{ID: "old", Scope: scope(), Kind: domain.KindState,
			Subject: "alice", Predicate: "city", Content: "Paris",
			ObservedAt: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Project(ctx, []domain.TemporalFact{
		{ID: "new", Scope: scope(), Kind: domain.KindState,
			Subject: "alice", Predicate: "city", Content: "Berlin",
			ObservedAt: time.Unix(2, 0)},
	}); err != nil {
		t.Fatal(err)
	}

	if err := p.Forget(ctx, scope(), []string{"old"}); err != nil {
		t.Fatal(err)
	}
	if got := p.Lookup(ctx, scope(), "alice"); !hasID(got, "new") {
		t.Fatalf("forgetting old fact must not remove current profile slot, got %+v", got)
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
