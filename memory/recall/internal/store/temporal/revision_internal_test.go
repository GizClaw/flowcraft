package temporal

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func revisionScope() domain.Scope { return domain.Scope{RuntimeID: "rt", UserID: "u1"} }

func revisionFact(id string, ts time.Time) domain.TemporalFact {
	return domain.TemporalFact{
		ID:         id,
		Scope:      revisionScope(),
		Kind:       domain.KindNote,
		Content:    "c-" + id,
		MergeKey:   "k-" + id,
		ObservedAt: ts,
	}
}

func TestFindByRevisionSource_ReturnsForkAndContest(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	a := revisionFact("a", time.Unix(10, 0))
	b := revisionFact("b", time.Unix(20, 0))
	domain.AttachRevision(&b, domain.Revision{Kind: domain.RevisionFork, SourceFactID: "a"})
	c := revisionFact("c", time.Unix(30, 0))
	domain.AttachRevision(&c, domain.Revision{Kind: domain.RevisionContest, SourceFactID: "a"})

	if err := s.Append(ctx, []domain.TemporalFact{a, b, c}); err != nil {
		t.Fatalf("append: %v", err)
	}

	got, err := s.FindByRevisionSource(ctx, revisionScope(), "a")
	if err != nil {
		t.Fatalf("FindByRevisionSource: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 descendants of a, got %d (%+v)", len(got), got)
	}
	if got[0].ID != "b" || got[1].ID != "c" {
		t.Errorf("want [b c] ordered by ObservedAt, got [%s %s]", got[0].ID, got[1].ID)
	}
}

func TestFindByRevisionSource_ScopePartition(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	other := domain.Scope{RuntimeID: "rt", UserID: "u2"}

	b := revisionFact("b", time.Unix(20, 0))
	domain.AttachRevision(&b, domain.Revision{Kind: domain.RevisionFork, SourceFactID: "a"})

	c := revisionFact("c", time.Unix(30, 0))
	c.Scope = other
	domain.AttachRevision(&c, domain.Revision{Kind: domain.RevisionFork, SourceFactID: "a"})

	if err := s.Append(ctx, []domain.TemporalFact{b, c}); err != nil {
		t.Fatalf("append: %v", err)
	}

	got, err := s.FindByRevisionSource(ctx, revisionScope(), "a")
	if err != nil {
		t.Fatalf("FindByRevisionSource: %v", err)
	}
	if len(got) != 1 || got[0].ID != "b" {
		t.Errorf("scope u1 must only return b, got %+v", got)
	}

	gotOther, err := s.FindByRevisionSource(ctx, other, "a")
	if err != nil {
		t.Fatalf("FindByRevisionSource(other): %v", err)
	}
	if len(gotOther) != 1 || gotOther[0].ID != "c" {
		t.Errorf("scope u2 must only return c, got %+v", gotOther)
	}
}
