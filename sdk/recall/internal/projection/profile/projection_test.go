package profile

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

func TestProfile_LookupBySubject(t *testing.T) {
	p := New()
	ctx := context.Background()
	facts := []model.TemporalFact{
		{ID: "s1", Scope: scope(), Kind: model.KindState,
			Subject: "alice", Predicate: "city", Content: "nyc",
			ObservedAt: time.Unix(1, 0)},
		{ID: "p1", Scope: scope(), Kind: model.KindPreference,
			Subject: "alice", Predicate: "food", Content: "sushi",
			ObservedAt: time.Unix(2, 0)},
		{ID: "r1", Scope: scope(), Kind: model.KindRelation,
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

func TestProfile_DropsExpiredSlot(t *testing.T) {
	p := New()
	ctx := context.Background()
	past := time.Unix(1, 0)
	f := model.TemporalFact{
		ID: "s1", Scope: scope(), Kind: model.KindState,
		Subject: "alice", Predicate: "city", Content: "nyc",
		ObservedAt: past, ValidTo: &past,
	}
	if err := p.Project(ctx, []model.TemporalFact{f}); err != nil {
		t.Fatal(err)
	}
	if got := p.Lookup(ctx, scope(), "alice"); len(got) != 0 {
		t.Fatalf("expired state must not be in profile, got %+v", got)
	}
}

func TestProfile_RebuildExactReplace(t *testing.T) {
	p := New()
	ctx := context.Background()
	if err := p.Project(ctx, []model.TemporalFact{
		{ID: "stale", Scope: scope(), Kind: model.KindState,
			Subject: "bob", Predicate: "city", ObservedAt: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Rebuild(ctx, scope(), []model.TemporalFact{
		{ID: "fresh", Scope: scope(), Kind: model.KindPreference,
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
	if err := p.Project(ctx, []model.TemporalFact{
		{ID: "agent-a", Scope: agentScope("agent-a"), Kind: model.KindState,
			Subject: "alice", Predicate: "city", Content: "Paris",
			ObservedAt: time.Unix(1, 0)},
		{ID: "agent-b", Scope: agentScope("agent-b"), Kind: model.KindState,
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
	if err := p.Project(ctx, []model.TemporalFact{
		{ID: "old", Scope: scope(), Kind: model.KindState,
			Subject: "alice", Predicate: "city", Content: "Paris",
			ObservedAt: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Project(ctx, []model.TemporalFact{
		{ID: "new", Scope: scope(), Kind: model.KindState,
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
