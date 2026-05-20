package ingest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

// fakeView is the resolver View backed by a slice of facts. It
// keeps tests free of the temporal store package.
type fakeView struct {
	facts []domain.TemporalFact
}

func (v *fakeView) FindByMergeKey(_ context.Context, scope domain.Scope, mergeKey string) ([]domain.TemporalFact, error) {
	if mergeKey == "" {
		return nil, nil
	}
	var out []domain.TemporalFact
	for _, f := range v.facts {
		if domain.ScopesMatch(f.Scope, scope) && f.MergeKey == mergeKey {
			out = append(out, f)
		}
	}
	return out, nil
}

func (v *fakeView) Get(_ context.Context, scope domain.Scope, factID string) (domain.TemporalFact, error) {
	for _, f := range v.facts {
		if domain.ScopesMatch(f.Scope, scope) && f.ID == factID {
			return f, nil
		}
	}
	return domain.TemporalFact{}, ErrNotInView
}

func TestResolver_SameMergeKeyIdenticalContentIsNoop(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt"}
	existing := domain.TemporalFact{
		ID: "old", Scope: scope, Kind: domain.KindState,
		Subject: "alice", Predicate: "city", Content: "Paris",
		MergeKey: "state|alice|city",
	}
	view := &fakeView{facts: []domain.TemporalFact{existing}}
	r := NewResolver()
	out, err := r.ResolveConflicts(context.Background(), view, []domain.TemporalFact{{
		ID: "new", Scope: scope, Kind: domain.KindState,
		Subject: "alice", Predicate: "city", Content: "Paris",
		MergeKey: "state|alice|city",
	}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(out.Facts) != 0 {
		t.Errorf("identical content must be noop, got %+v", out.Facts)
	}
	if len(out.Drops) != 1 || out.Drops[0].Reason != "conflict:duplicate_content" {
		t.Errorf("drops = %+v", out.Drops)
	}
	if len(out.Closes) != 0 {
		t.Errorf("noop must not close anything, got %+v", out.Closes)
	}
}

func TestResolver_StateSupersedesOnChange(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt"}
	existing := domain.TemporalFact{
		ID: "old", Scope: scope, Kind: domain.KindState,
		Subject: "alice", Predicate: "city", Content: "Paris",
		MergeKey:   "state|alice|city",
		ObservedAt: time.Unix(1, 0),
	}
	view := &fakeView{facts: []domain.TemporalFact{existing}}
	r := &DefaultResolver{Clock: func() time.Time { return time.Unix(100, 0) }}
	out, err := r.ResolveConflicts(context.Background(), view, []domain.TemporalFact{{
		ID: "new", Scope: scope, Kind: domain.KindState,
		Subject: "alice", Predicate: "city", Content: "Berlin",
		MergeKey:   "state|alice|city",
		ObservedAt: time.Unix(50, 0),
	}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(out.Facts) != 1 {
		t.Fatalf("want 1 fact, got %+v", out.Facts)
	}
	if got := out.Facts[0].Supersedes; len(got) != 1 || got[0] != "old" {
		t.Errorf("supersedes = %v", got)
	}
	if len(out.Closes) != 1 {
		t.Fatalf("want 1 close, got %+v", out.Closes)
	}
	cl := out.Closes[0]
	if cl.FactID != "old" || cl.CorrectedBy != "new" || !cl.ValidTo.Equal(time.Unix(50, 0)) {
		t.Errorf("close instruction = %+v", cl)
	}
}

func TestResolver_StateSupersedeChainsWithinBatch(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt"}
	existing := domain.TemporalFact{
		ID: "old", Scope: scope, Kind: domain.KindState,
		Subject: "alice", Predicate: "city", Content: "Paris",
		MergeKey:   "state|alice|city",
		ObservedAt: time.Unix(1, 0),
	}
	view := &fakeView{facts: []domain.TemporalFact{existing}}
	r := &DefaultResolver{Clock: func() time.Time { return time.Unix(100, 0) }}
	out, err := r.ResolveConflicts(context.Background(), view, []domain.TemporalFact{
		{
			ID: "new1", Scope: scope, Kind: domain.KindState,
			Subject: "alice", Predicate: "city", Content: "Berlin",
			MergeKey:   "state|alice|city",
			ObservedAt: time.Unix(50, 0),
		},
		{
			ID: "new2", Scope: scope, Kind: domain.KindState,
			Subject: "alice", Predicate: "city", Content: "Rome",
			MergeKey:   "state|alice|city",
			ObservedAt: time.Unix(51, 0),
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(out.Facts) != 2 {
		t.Fatalf("want 2 facts, got %+v", out.Facts)
	}
	if got := out.Facts[0].Supersedes; len(got) != 1 || got[0] != "old" {
		t.Fatalf("first supersedes = %v, want [old]", got)
	}
	if got := out.Facts[1].Supersedes; len(got) != 1 || got[0] != "new1" {
		t.Fatalf("second supersedes = %v, want [new1]", got)
	}
	if len(out.Closes) != 2 {
		t.Fatalf("want 2 closes, got %+v", out.Closes)
	}
	if out.Closes[0].FactID != "old" || out.Closes[0].CorrectedBy != "new1" {
		t.Fatalf("first close = %+v", out.Closes[0])
	}
	if out.Closes[1].FactID != "new1" || out.Closes[1].CorrectedBy != "new2" {
		t.Fatalf("second close = %+v", out.Closes[1])
	}
}

func TestResolver_PreferenceSupersedesOnChange(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt"}
	existing := domain.TemporalFact{
		ID: "p1", Scope: scope, Kind: domain.KindPreference,
		Subject: "alice", Predicate: "favourite_color", Content: "blue",
		MergeKey: "preference|alice|favourite_color",
	}
	view := &fakeView{facts: []domain.TemporalFact{existing}}
	r := NewResolver()
	out, _ := r.ResolveConflicts(context.Background(), view, []domain.TemporalFact{{
		ID: "p2", Scope: scope, Kind: domain.KindPreference,
		Subject: "alice", Predicate: "favourite_color", Content: "green",
		MergeKey: "preference|alice|favourite_color",
	}})
	if len(out.Facts) != 1 || len(out.Closes) != 1 {
		t.Fatalf("preference supersede failed: %+v / %+v", out.Facts, out.Closes)
	}
	if out.Closes[0].FactID != "p1" {
		t.Errorf("wrong close target: %+v", out.Closes[0])
	}
}

func TestResolver_EventIsAppendOnly(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt"}
	existing := domain.TemporalFact{
		ID: "e1", Scope: scope, Kind: domain.KindEvent,
		Content: "ate ramen", MergeKey: "event|||ate-ramen-hash",
	}
	view := &fakeView{facts: []domain.TemporalFact{existing}}
	r := NewResolver()
	out, _ := r.ResolveConflicts(context.Background(), view, []domain.TemporalFact{{
		ID: "e2", Scope: scope, Kind: domain.KindEvent,
		Content: "ate ramen", MergeKey: "event|||ate-ramen-hash",
	}})
	if len(out.Facts) != 1 || out.Facts[0].ID != "e2" {
		t.Errorf("events must be append-only: %+v", out.Facts)
	}
	if len(out.Closes) != 0 {
		t.Errorf("events must never close prior facts, got %+v", out.Closes)
	}
}

func TestResolver_RelationDifferentObjectAppends(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt"}
	existing := domain.TemporalFact{
		ID: "r1", Scope: scope, Kind: domain.KindRelation,
		Subject: "alice", Predicate: "spouse", Object: "bob",
		MergeKey: "relation|alice|spouse|bob",
	}
	view := &fakeView{facts: []domain.TemporalFact{existing}}
	r := NewResolver()
	out, _ := r.ResolveConflicts(context.Background(), view, []domain.TemporalFact{{
		ID: "r2", Scope: scope, Kind: domain.KindRelation,
		Subject: "alice", Predicate: "spouse", Object: "carol",
		MergeKey: "relation|alice|spouse|carol",
	}})
	if len(out.Facts) != 1 || out.Facts[0].ID != "r2" {
		t.Errorf("different relation object must append: %+v", out.Facts)
	}
	if len(out.Closes) != 0 {
		t.Errorf("different relation object must not close prior: %+v", out.Closes)
	}
}

func TestResolver_NoteDedupesByContent(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt"}
	existing := domain.TemporalFact{
		ID: "n1", Scope: scope, Kind: domain.KindNote,
		Content: "buy milk", MergeKey: "note|hash",
	}
	view := &fakeView{facts: []domain.TemporalFact{existing}}
	r := NewResolver()
	out, _ := r.ResolveConflicts(context.Background(), view, []domain.TemporalFact{{
		ID: "n2", Scope: scope, Kind: domain.KindNote,
		Content: "buy milk", MergeKey: "note|hash",
	}})
	if len(out.Facts) != 0 {
		t.Errorf("duplicate note must be noop, got %+v", out.Facts)
	}
}

func TestResolver_NilViewPassthrough(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt"}
	r := NewResolver()
	in := []domain.TemporalFact{
		{ID: "x", Scope: scope, Kind: domain.KindNote, Content: "hi", MergeKey: "k"},
	}
	out, err := r.ResolveConflicts(context.Background(), nil, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Facts) != 1 {
		t.Errorf("nil view must not suppress facts, got %+v", out.Facts)
	}
}

func TestResolver_PropagatesViewLookupError(t *testing.T) {
	r := NewResolver()
	_, err := r.ResolveConflicts(context.Background(), errView{err: errors.New("store unavailable")}, []domain.TemporalFact{{
		ID:        "new",
		Scope:     domain.Scope{RuntimeID: "rt"},
		Kind:      domain.KindState,
		Subject:   "alice",
		Predicate: "city",
		Content:   "Paris",
		MergeKey:  "state|alice|city",
	}})
	if err == nil {
		t.Fatal("store lookup errors must abort conflict resolution, not degrade to append")
	}
}

type errView struct {
	err error
}

func (v errView) FindByMergeKey(context.Context, domain.Scope, string) ([]domain.TemporalFact, error) {
	return nil, v.err
}

func (v errView) Get(context.Context, domain.Scope, string) (domain.TemporalFact, error) {
	return domain.TemporalFact{}, v.err
}
