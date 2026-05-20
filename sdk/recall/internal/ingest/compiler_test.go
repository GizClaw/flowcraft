package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/governance"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

func TestCompile_FillsDeterministicFields(t *testing.T) {
	cp := New(Stages{
		IDGen: SequentialIDGenerator("fct_"),
		Clock: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	})
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt", UserID: "u1"},
		Facts: []domain.TemporalFact{{
			Kind:      domain.KindRelation,
			Subject:   "Alice",
			Predicate: "spouse",
			Object:    "Bob",
		}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 1 {
		t.Fatalf("want 1 fact, got %d", len(res.Facts))
	}
	got := res.Facts[0]
	if got.ID != "fct_000001" {
		t.Errorf("id = %q, want fct_000001", got.ID)
	}
	if got.Scope.RuntimeID != "rt" || got.Scope.UserID != "u1" {
		t.Errorf("scope not propagated: %+v", got.Scope)
	}
	if got.ObservedAt.IsZero() {
		t.Error("observed_at not filled")
	}
	if got.MergeKey != "relation|alice|spouse|bob" {
		t.Errorf("merge_key = %q, want relation|alice|spouse|bob", got.MergeKey)
	}
	if got.Confidence != DefaultConfidence {
		t.Errorf("confidence = %v, want %v", got.Confidence, DefaultConfidence)
	}
	// EntityResolver should have added alice/bob to entities.
	want := map[string]bool{"alice": true, "bob": true}
	for _, e := range got.Entities {
		delete(want, e)
	}
	if len(want) != 0 {
		t.Errorf("entities missing: %v (have %v)", want, got.Entities)
	}
}

func TestCompile_RelationMergeKeyDifferentiatesObjects(t *testing.T) {
	cp := New(Stages{IDGen: SequentialIDGenerator("f")})
	mk := func(object string) string {
		res, err := cp.Compile(context.Background(), port.IngestInput{
			Scope: domain.Scope{RuntimeID: "rt"},
			Facts: []domain.TemporalFact{{
				Kind:      domain.KindRelation,
				Subject:   "Alice",
				Predicate: "spouse",
				Object:    object,
			}},
		})
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		return res.Facts[0].MergeKey
	}
	a := mk("Bob")
	b := mk("Carol")
	if a == b {
		t.Fatalf("relation merge keys must differ by object; got %q for both", a)
	}
}

func TestCompile_StateMergeKeyDedupes(t *testing.T) {
	cp := New(Stages{IDGen: SequentialIDGenerator("f")})
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Facts: []domain.TemporalFact{
			{Kind: domain.KindState, Subject: "Alice", Predicate: "city", Content: "Paris"},
			{Kind: domain.KindState, Subject: "alice", Predicate: "CITY", Content: "Berlin"},
		},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 2 {
		t.Fatalf("want 2 facts, got %d", len(res.Facts))
	}
	if res.Facts[0].MergeKey != res.Facts[1].MergeKey {
		t.Errorf("normalized state merge keys should match: %q vs %q",
			res.Facts[0].MergeKey, res.Facts[1].MergeKey)
	}
}

func TestCompile_RejectsInvalidKind(t *testing.T) {
	cp := Default()
	_, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Facts: []domain.TemporalFact{{Kind: "ufo", Content: "x"}},
	})
	if err == nil {
		t.Fatal("want error for invalid kind")
	}
}

func TestCompile_RequiresScope(t *testing.T) {
	cp := Default()
	_, err := cp.Compile(context.Background(), port.IngestInput{
		Facts: []domain.TemporalFact{{Kind: domain.KindNote, Content: "x"}},
	})
	if err == nil {
		t.Fatal("want error for missing scope")
	}
}

func TestCompile_PolicyRejectDrops(t *testing.T) {
	cp := New(Stages{
		IDGen:  SequentialIDGenerator("f"),
		Policy: rejectAllPolicy{},
	})
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Facts: []domain.TemporalFact{{Kind: domain.KindNote, Content: "secret"}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 0 {
		t.Errorf("want 0 facts after policy reject, got %d", len(res.Facts))
	}
	if len(res.Dropped) != 1 {
		t.Errorf("want 1 dropped fact, got %d", len(res.Dropped))
	}
}

func TestCompile_GovernanceMutationPrecedesDerivedFields(t *testing.T) {
	cp := New(Stages{
		IDGen: SequentialIDGenerator("f"),
		Governance: &governance.Governance{
			Write: mutateContentPolicy{content: "redacted content"},
		},
	})
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Facts: []domain.TemporalFact{{Kind: domain.KindNote, Content: "secret content"}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 1 {
		t.Fatalf("want 1 fact, got %d", len(res.Facts))
	}
	got := res.Facts[0]
	if got.Content != "redacted content" {
		t.Fatalf("content = %q", got.Content)
	}
	if got.MergeKey != DefaultMergeKey(got) {
		t.Fatalf("merge_key = %q, want derived from mutated fact %q", got.MergeKey, DefaultMergeKey(got))
	}
}

type rejectAllPolicy struct{}

func (rejectAllPolicy) Apply(f domain.TemporalFact) (domain.TemporalFact, bool) { return f, false }

type mutateContentPolicy struct {
	content string
}

func (p mutateContentPolicy) Apply(f domain.TemporalFact) (domain.TemporalFact, bool) {
	f.Content = p.content
	return f, true
}
