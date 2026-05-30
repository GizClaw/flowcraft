package stages_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/revision"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/revision/stages"
)

// TestSave_DelegatesToSaveFnAndCapturesCreated pins the headline
// contract: the stage invokes the injected SaveFn with the
// attach-prepared fact and records the returned canonical fact into
// state.Created so the facade can hand it back.
func TestSave_DelegatesToSaveFnAndCapturesCreated(t *testing.T) {
	wantID := "saved-1"
	saveFn := func(_ context.Context, _ domain.Scope, fact domain.TemporalFact) (domain.TemporalFact, error) {
		fact.ID = wantID
		return fact, nil
	}
	stage := stages.NewSave(saveFn, nil)
	state := &revision.State{
		Scope:        domain.Scope{RuntimeID: "rt", UserID: "u"},
		Mode:         revision.ModeFork,
		SourceFactID: "src",
		NewFact:      domain.TemporalFact{Kind: domain.KindNote, Content: "x"},
	}
	d, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.Created.ID != wantID {
		t.Errorf("Created.ID = %q, want %q", state.Created.ID, wantID)
	}
	det, ok := d.(diagnostic.RevisionDetail)
	if !ok {
		t.Fatalf("detail = %T, want RevisionDetail", d)
	}
	if det.CreatedFactID != wantID {
		t.Errorf("detail.CreatedFactID = %q, want %q", det.CreatedFactID, wantID)
	}
}

// TestSave_SaveFnErrorPropagates pins that any save failure surfaces
// as a stage error so the framework records Status=Failed and the
// caller sees the underlying cause.
func TestSave_SaveFnErrorPropagates(t *testing.T) {
	boom := errors.New("save boom")
	saveFn := func(context.Context, domain.Scope, domain.TemporalFact) (domain.TemporalFact, error) {
		return domain.TemporalFact{}, boom
	}
	stage := stages.NewSave(saveFn, nil)
	state := &revision.State{
		Scope:        domain.Scope{RuntimeID: "rt", UserID: "u"},
		Mode:         revision.ModeFork,
		SourceFactID: "src",
		NewFact:      domain.TemporalFact{Kind: domain.KindNote, Content: "x"},
	}
	if _, err := stage.Run(context.Background(), state); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}
