package stages_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write/stages"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
)

func TestValidityClose_RunSucceeds(t *testing.T) {
	store := &fakeStore{}
	s := stages.NewValidityClose(store, nil, nil)
	state := &write.WriteState{Resolution: domain.Resolution{Closes: []domain.ValidityClose{
		{FactID: "p1", CorrectedBy: "n1"},
		{FactID: "p2", CorrectedBy: "n2"},
	}}}
	d, err := s.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := d.(diagnostic.ValidityCloseDetail).ClosedFacts; got != 2 {
		t.Errorf("ClosedFacts = %d", got)
	}
	if len(state.AppliedCloses) != 2 {
		t.Errorf("AppliedCloses = %+v", state.AppliedCloses)
	}
}

func TestValidityClose_TolersErrValidityAlreadyClosed(t *testing.T) {
	store := &fakeStore{updateValid: map[string]error{
		"p2": temporalstore.ErrValidityAlreadyClosed,
	}}
	s := stages.NewValidityClose(store, nil, nil)
	state := &write.WriteState{Resolution: domain.Resolution{Closes: []domain.ValidityClose{
		{FactID: "p1", CorrectedBy: "n1"},
		{FactID: "p2", CorrectedBy: "n2"},
	}}}
	if _, err := s.Run(context.Background(), state); err != nil {
		t.Fatalf("benign close must not fail: %v", err)
	}
	if len(state.AppliedCloses) != 1 || state.AppliedCloses[0].FactID != "p1" {
		t.Errorf("AppliedCloses = %+v", state.AppliedCloses)
	}
}

func TestValidityClose_HardErrorRecordsPrefix(t *testing.T) {
	boom := errors.New("update boom")
	store := &fakeStore{updateValid: map[string]error{"p2": boom}}
	s := stages.NewValidityClose(store, nil, nil)
	state := &write.WriteState{Resolution: domain.Resolution{Closes: []domain.ValidityClose{
		{FactID: "p1", CorrectedBy: "n1"},
		{FactID: "p2", CorrectedBy: "n2"},
	}}}
	_, err := s.Run(context.Background(), state)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if len(state.AppliedCloses) != 1 || state.AppliedCloses[0].FactID != "p1" {
		t.Errorf("AppliedCloses prefix wrong: %+v", state.AppliedCloses)
	}
	if state.FailedStage != "validity_close" {
		t.Errorf("FailedStage = %q", state.FailedStage)
	}
}

func TestValidityClose_CompensateReopens(t *testing.T) {
	store := &fakeStore{}
	hook := &recordHook{}
	s := stages.NewValidityClose(store, nil, hook)
	state := &write.WriteState{
		AppliedCloses: []domain.ValidityClose{{FactID: "p1", CorrectedBy: "n1"}},
	}
	_ = s.Compensate(context.Background(), state)
	if len(store.reopened) != 1 || store.reopened[0] != "p1" {
		t.Errorf("reopened = %v", store.reopened)
	}
}

// TestApply_NPriorsAllClosed covers the D1 (2026-05-21) N:1
// supersede surface inside this stage: when state.Resolution.Closes
// carries N entries from a single successor fact, every one of
// them must be issued as an UpdateValidity call and every one must
// land in state.AppliedCloses (so compensation can reopen them
// later).
func TestApply_NPriorsAllClosed(t *testing.T) {
	store := &fakeStore{}
	s := stages.NewValidityClose(store, nil, nil)
	state := &write.WriteState{Resolution: domain.Resolution{Closes: []domain.ValidityClose{
		{FactID: "a", CorrectedBy: "summary"},
		{FactID: "b", CorrectedBy: "summary"},
		{FactID: "c", CorrectedBy: "summary"},
	}}}
	d, err := s.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := d.(diagnostic.ValidityCloseDetail).ClosedFacts; got != 3 {
		t.Errorf("ClosedFacts = %d, want 3", got)
	}
	if len(state.AppliedCloses) != 3 {
		t.Fatalf("AppliedCloses = %+v, want 3", state.AppliedCloses)
	}
	for i, want := range []string{"a", "b", "c"} {
		if state.AppliedCloses[i].FactID != want {
			t.Errorf("AppliedCloses[%d].FactID = %q, want %q", i, state.AppliedCloses[i].FactID, want)
		}
		if state.AppliedCloses[i].CorrectedBy != "summary" {
			t.Errorf("AppliedCloses[%d].CorrectedBy = %q, want summary", i, state.AppliedCloses[i].CorrectedBy)
		}
	}
}

func TestValidityClose_CompensateToleratesReopenErr(t *testing.T) {
	store := &fakeStore{reopenErr: errors.New("conflict")}
	s := stages.NewValidityClose(store, nil, nil)
	state := &write.WriteState{
		AppliedCloses: []domain.ValidityClose{{FactID: "p1", CorrectedBy: "n1"}},
	}
	if err := s.Compensate(context.Background(), state); err != nil {
		t.Fatalf("reopen err must not fail compensate: %v", err)
	}
}
