package ingest

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func TestResolver_ForkDoesNotClosePrior(t *testing.T) {
	r := NewResolver()
	view := emptyView{}
	prior := domain.TemporalFact{
		ID:       "old",
		Scope:    domain.Scope{RuntimeID: "rt"},
		Kind:     domain.KindState,
		MergeKey: "state:avery:location",
		Content:  "riverton",
	}
	fork := domain.TemporalFact{
		ID:       "new",
		Scope:    prior.Scope,
		Kind:     domain.KindState,
		MergeKey: "state:avery:location:fork",
		Content:  "lyon",
	}
	domain.AttachRevision(&fork, domain.Revision{Kind: domain.RevisionFork, SourceFactID: "old"})
	out, err := r.ResolveConflicts(context.Background(), view, []domain.TemporalFact{fork})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Facts) != 1 || len(out.Closes) != 0 {
		t.Fatalf("fork resolve: facts=%d closes=%d", len(out.Facts), len(out.Closes))
	}
	_ = prior
}
