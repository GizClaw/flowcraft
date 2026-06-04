package relation

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func scope() domain.Scope { return domain.Scope{RuntimeID: "rt", UserID: "u1"} }

func agentScope(agentID string) domain.Scope {
	s := scope()
	s.AgentID = agentID
	return s
}

// TestRelation_IndexesEventFactWithTriple pins the relation projection
// invariant: structurized event / state / preference facts that carry a
// meaningful (Subject, Predicate, Object) triple must be indexed even
// when Kind is not Relation. Otherwise typed-relation recall loses
// facts that are semantically relational but classified by lifecycle
// kind.
func TestRelation_IndexesEventFactWithTriple(t *testing.T) {
	p := New()
	ctx := context.Background()
	future := time.Now().Add(24 * time.Hour)
	f := domain.TemporalFact{
		ID: "ev1", Scope: scope(),
		Kind:    domain.KindEvent, // not KindRelation
		Subject: "Avery", Predicate: "read", Object: "Charlotte's Web",
		ObservedAt: time.Now(),
		ValidTo:    &future, // keep projectable for the active-slot view
	}
	if err := p.Project(ctx, []domain.TemporalFact{f}); err != nil {
		t.Fatal(err)
	}
	if got := p.Lookup(ctx, scope(), "avery", "read", ""); len(got) != 1 || got[0] != "ev1" {
		t.Fatalf("event-fact triple lookup = %+v, want [ev1]", got)
	}
	if got := p.Lookup(ctx, scope(), "avery", "", ""); len(got) != 1 || got[0] != "ev1" {
		t.Fatalf("subject-only lookup over event fact = %+v, want [ev1]", got)
	}
}

func TestRelation_DropsIncompleteTriples(t *testing.T) {
	p := New()
	ctx := context.Background()
	future := time.Now().Add(24 * time.Hour)
	facts := []domain.TemporalFact{
		{ID: "subject-only", Scope: scope(), Kind: domain.KindState, Subject: "Avery", ObservedAt: time.Now(), ValidTo: &future},
		{ID: "predicate-only", Scope: scope(), Kind: domain.KindState, Predicate: "read", ObservedAt: time.Now(), ValidTo: &future},
		{ID: "object-only", Scope: scope(), Kind: domain.KindState, Object: "The Glass Compass", ObservedAt: time.Now(), ValidTo: &future},
		{ID: "complete", Scope: scope(), Kind: domain.KindState, Subject: "Avery", Predicate: "read", Object: "The Glass Compass", ObservedAt: time.Now(), ValidTo: &future},
	}
	if err := p.Project(ctx, facts); err != nil {
		t.Fatal(err)
	}
	if got := p.Lookup(ctx, scope(), "avery", "", ""); len(got) != 1 || got[0] != "complete" {
		t.Fatalf("relation projection should index only complete triples, got %+v", got)
	}
	if got := p.Lookup(ctx, scope(), "", "read", ""); len(got) != 1 || got[0] != "complete" {
		t.Fatalf("predicate lookup should only see complete triple, got %+v", got)
	}
}

func TestRelation_DoesNotIndexParameterFacts(t *testing.T) {
	p := New()
	ctx := context.Background()
	future := time.Now().Add(24 * time.Hour)
	f := domain.TemporalFact{
		ID: "param1", Scope: scope(),
		Kind:       domain.KindParameter,
		Subject:    "experiment",
		Predicate:  "parameter_value",
		Object:     "0.2",
		ObservedAt: time.Now(),
		ValidTo:    &future,
	}
	if p.AcceptsKind(domain.KindParameter) {
		t.Fatal("relation projection must explicitly reject KindParameter")
	}
	if err := p.Project(ctx, []domain.TemporalFact{f}); err != nil {
		t.Fatal(err)
	}
	if got := p.Lookup(ctx, scope(), "experiment", "parameter_value", "0.2"); len(got) != 0 {
		t.Fatalf("parameter fact polluted relation projection: %+v", got)
	}
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

func TestRelation_LookupLimitBoundsAndSortsIDs(t *testing.T) {
	p := New()
	ctx := context.Background()
	facts := []domain.TemporalFact{
		{ID: "rel-c", Scope: scope(), Kind: domain.KindRelation, Subject: "alice", Predicate: "visited", Object: "cairo", ObservedAt: time.Unix(1, 0)},
		{ID: "rel-a", Scope: scope(), Kind: domain.KindRelation, Subject: "alice", Predicate: "visited", Object: "athens", ObservedAt: time.Unix(1, 0)},
		{ID: "rel-d", Scope: scope(), Kind: domain.KindRelation, Subject: "alice", Predicate: "visited", Object: "delhi", ObservedAt: time.Unix(1, 0)},
		{ID: "rel-b", Scope: scope(), Kind: domain.KindRelation, Subject: "alice", Predicate: "visited", Object: "berlin", ObservedAt: time.Unix(1, 0)},
	}
	if err := p.Project(ctx, facts); err != nil {
		t.Fatal(err)
	}

	got := p.LookupLimit(ctx, scope(), "alice", "", "", 2)
	want := []string{"rel-a", "rel-b"}
	if len(got) != len(want) {
		t.Fatalf("LookupLimit len = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LookupLimit order = %+v, want %+v", got, want)
		}
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

// TestRelation_DropsClosed pins that soft-forgotten (Closed)
// relations must not survive in the projection cache. Before the
// predicate split, the projection used IsCanonicalActive (canonical only)
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
