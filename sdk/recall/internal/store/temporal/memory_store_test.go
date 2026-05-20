package temporal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

func scope() domain.Scope { return domain.Scope{RuntimeID: "rt", UserID: "u1"} }

func sampleFact(id, mergeKey string, kind domain.FactKind, ts time.Time, entities ...string) domain.TemporalFact {
	return domain.TemporalFact{
		ID:         id,
		Scope:      scope(),
		Kind:       kind,
		Content:    "c-" + id,
		MergeKey:   mergeKey,
		Entities:   entities,
		ObservedAt: ts,
	}
}

func TestAppend_RejectsDuplicateID(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	f := sampleFact("a", "k", domain.KindNote, time.Unix(1, 0))
	if err := s.Append(ctx, []domain.TemporalFact{f}); err != nil {
		t.Fatalf("first append: %v", err)
	}
	err := s.Append(ctx, []domain.TemporalFact{f})
	if err == nil {
		t.Fatal("want error on duplicate id")
	}
}

func TestAppend_RejectsInvalidKindAndScope(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.Append(ctx, []domain.TemporalFact{{ID: "x", Scope: scope(), Kind: "bogus"}}); err == nil {
		t.Error("want error on invalid kind")
	}
	if err := s.Append(ctx, []domain.TemporalFact{{ID: "x", Kind: domain.KindNote}}); err == nil {
		t.Error("want error on missing scope.runtime_id")
	}
}

func TestList_HidesSupersededByDefault(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	t1 := time.Unix(10, 0)
	t2 := time.Unix(20, 0)
	a := sampleFact("a", "kx", domain.KindState, t1)
	b := sampleFact("b", "kx", domain.KindState, t2)
	if err := s.Append(ctx, []domain.TemporalFact{a, b}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateValidity(ctx, scope(), "a", t2, "b"); err != nil {
		t.Fatalf("update validity: %v", err)
	}
	res, err := s.List(ctx, scope(), ListQuery{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res) != 1 || res[0].ID != "b" {
		t.Errorf("want only b, got %+v", res)
	}
	all, err := s.List(ctx, scope(), ListQuery{IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("want 2 with IncludeSuperseded, got %d", len(all))
	}
}

func TestList_DoesNotHideClosedValidityWithoutCorrection(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	validTo := time.Unix(20, 0)
	f := sampleFact("a", "event|a", domain.KindEvent, time.Unix(10, 0))
	f.ValidTo = &validTo
	if err := s.Append(ctx, []domain.TemporalFact{f}); err != nil {
		t.Fatal(err)
	}
	got, err := s.List(ctx, scope(), ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("closed validity without corrected_by must remain visible, got %+v", got)
	}
}

func TestAppend_RejectsDuplicateIDsWithinBatch(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	a := sampleFact("dup", "k1", domain.KindNote, time.Unix(1, 0))
	b := sampleFact("dup", "k2", domain.KindNote, time.Unix(2, 0))
	if err := s.Append(ctx, []domain.TemporalFact{a, b}); err == nil {
		t.Fatal("want error on duplicate fact id within one append batch")
	}
	got, err := s.List(ctx, scope(), ListQuery{IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("failed append batch must not partially commit facts: %+v", got)
	}
}

func TestList_FilterKindAndEntities(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	t1 := time.Unix(1, 0)
	a := sampleFact("a", "k", domain.KindState, t1, "alice")
	b := sampleFact("b", "k2", domain.KindNote, t1, "alice", "bob")
	c := sampleFact("c", "k3", domain.KindState, t1, "carol")
	if err := s.Append(ctx, []domain.TemporalFact{a, b, c}); err != nil {
		t.Fatal(err)
	}

	states, err := s.List(ctx, scope(), ListQuery{Kinds: []domain.FactKind{domain.KindState}})
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 {
		t.Errorf("want 2 state facts, got %d", len(states))
	}

	alice, err := s.List(ctx, scope(), ListQuery{Entities: []string{"alice"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(alice) != 2 {
		t.Errorf("want 2 facts mentioning alice, got %d", len(alice))
	}

	both, err := s.List(ctx, scope(), ListQuery{Entities: []string{"alice", "bob"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 1 || both[0].ID != "b" {
		t.Errorf("want only b, got %+v", both)
	}
}

func TestFindByMergeKeyAndSupersededBy(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	a := sampleFact("a", "k", domain.KindState, time.Unix(1, 0))
	b := sampleFact("b", "k", domain.KindState, time.Unix(2, 0))
	if err := s.Append(ctx, []domain.TemporalFact{a, b}); err != nil {
		t.Fatal(err)
	}
	got, err := s.FindByMergeKey(ctx, scope(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("FindByMergeKey: %+v", got)
	}

	if err := s.UpdateValidity(ctx, scope(), "a", time.Unix(2, 0), "b"); err != nil {
		t.Fatal(err)
	}
	sup, err := s.FindSupersededBy(ctx, scope(), "b")
	if err != nil {
		t.Fatal(err)
	}
	if len(sup) != 1 || sup[0].ID != "a" {
		t.Errorf("FindSupersededBy: %+v", sup)
	}

	empty, err := s.FindByMergeKey(ctx, scope(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Error("empty merge_key must not enumerate scope")
	}
}

func TestUpdateValidity_Idempotent(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.Append(ctx, []domain.TemporalFact{sampleFact("a", "k", domain.KindState, time.Unix(1, 0))}); err != nil {
		t.Fatal(err)
	}
	vt := time.Unix(100, 0)
	if err := s.UpdateValidity(ctx, scope(), "a", vt, "b"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateValidity(ctx, scope(), "a", vt, "b"); err != nil {
		t.Errorf("idempotent re-close must succeed: %v", err)
	}
	if err := s.UpdateValidity(ctx, scope(), "a", time.Unix(200, 0), "c"); err == nil {
		t.Error("re-closing with a different validity must fail")
	}
	if err := s.UpdateValidity(ctx, scope(), "missing", vt, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestDelete_RemovesIndexEntries(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	a := sampleFact("a", "k", domain.KindState, time.Unix(1, 0))
	b := sampleFact("b", "k", domain.KindState, time.Unix(2, 0))
	if err := s.Append(ctx, []domain.TemporalFact{a, b}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, scope(), []string{"a", "missing"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, scope(), "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a should be gone, got %v", err)
	}
	got, err := s.FindByMergeKey(ctx, scope(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "b" {
		t.Errorf("merge_key index not pruned: %+v", got)
	}
}

func TestStore_IsolatesScopes(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	a := sampleFact("a", "k", domain.KindNote, time.Unix(1, 0))
	other := domain.Scope{RuntimeID: "rt", UserID: "u2"}
	b := a
	b.Scope = other
	if err := s.Append(ctx, []domain.TemporalFact{a, b}); err != nil {
		t.Fatal(err)
	}
	gotA, err := s.List(ctx, scope(), ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotA) != 1 {
		t.Errorf("scope u1 should hold one fact, got %d", len(gotA))
	}
	gotB, err := s.List(ctx, other, ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotB) != 1 {
		t.Errorf("scope u2 should hold one fact, got %d", len(gotB))
	}
}

func TestStore_DoesNotPartitionByAgentID(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	base := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	a := sampleFact("a", "ka", domain.KindNote, time.Unix(1, 0))
	a.Scope = domain.Scope{RuntimeID: base.RuntimeID, UserID: base.UserID, AgentID: "agent-a"}
	b := sampleFact("b", "kb", domain.KindNote, time.Unix(2, 0))
	b.Scope = domain.Scope{RuntimeID: base.RuntimeID, UserID: base.UserID, AgentID: "agent-b"}
	if err := s.Append(ctx, []domain.TemporalFact{a, b}); err != nil {
		t.Fatal(err)
	}
	got, err := s.List(ctx, domain.Scope{RuntimeID: base.RuntimeID, UserID: base.UserID, AgentID: "agent-a"}, ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("AgentID is soft isolation metadata and must not partition the ledger, got %+v", got)
	}
}
