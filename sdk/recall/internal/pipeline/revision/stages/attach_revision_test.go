package stages_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/revision"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/revision/stages"
)

// TestAttachRevision_ForkStampsScopeAndRevision pins the Fork
// attach contract: caller draft → scoped + Revision-stamped fact
// ready for the canonical write pipeline. MergeKey defaults derived
// from the source so retrieval keeps fork branches grouped.
func TestAttachRevision_ForkStampsScopeAndRevision(t *testing.T) {
	stage := stages.NewAttachRevision()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	state := &revision.State{
		Scope:        scope,
		Mode:         revision.ModeFork,
		SourceFactID: "src",
		Source:       domain.TemporalFact{ID: "src", MergeKey: "alice|city"},
		NewFact: domain.TemporalFact{
			Kind:    domain.KindState,
			Content: "alice in lyon",
		},
	}
	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.NewFact.Scope.RuntimeID != scope.RuntimeID || state.NewFact.Scope.UserID != scope.UserID {
		t.Errorf("Scope not stamped: %+v", state.NewFact.Scope)
	}
	if state.NewFact.MergeKey != "alice|city:fork" {
		t.Errorf("MergeKey = %q, want alice|city:fork", state.NewFact.MergeKey)
	}
	rev, ok := domain.RevisionOf(state.NewFact)
	if !ok || rev.Kind != domain.RevisionFork || rev.SourceFactID != "src" {
		t.Errorf("Revision = %+v ok=%v, want fork/src", rev, ok)
	}
}

// TestAttachRevision_ContestBuildsNote pins the Contest attach
// contract: caller supplies note + evidence, the stage materialises
// a FactNote anchored on SourceFactID via RevisionContest.
func TestAttachRevision_ContestBuildsNote(t *testing.T) {
	stage := stages.NewAttachRevision()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u"}
	state := &revision.State{
		Scope:        scope,
		Mode:         revision.ModeContest,
		SourceFactID: "src",
		Source:       domain.TemporalFact{ID: "src"},
		Note:         "data is stale",
		Evidence:     []domain.EvidenceRef{{ID: "ev-1"}},
	}
	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.NewFact.Kind != domain.KindNote {
		t.Errorf("Kind = %v, want KindNote", state.NewFact.Kind)
	}
	if state.NewFact.Content != "data is stale" {
		t.Errorf("Content = %q", state.NewFact.Content)
	}
	if len(state.NewFact.EvidenceRefs) != 1 || state.NewFact.EvidenceRefs[0].ID != "ev-1" {
		t.Errorf("EvidenceRefs = %+v", state.NewFact.EvidenceRefs)
	}
	rev, ok := domain.RevisionOf(state.NewFact)
	if !ok || rev.Kind != domain.RevisionContest || rev.SourceFactID != "src" {
		t.Errorf("Revision = %+v ok=%v, want contest/src", rev, ok)
	}
}
