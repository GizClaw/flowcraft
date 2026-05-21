package stages_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write/stages"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

type fakeStore struct {
	appended    []domain.TemporalFact
	deleted     []string
	reopened    []string
	appendErr   error
	deleteErr   error
	reopenErr   error
	updateValid map[string]error
}

func (s *fakeStore) Append(_ context.Context, facts []domain.TemporalFact) error {
	if s.appendErr != nil {
		return s.appendErr
	}
	s.appended = append(s.appended, facts...)
	return nil
}

func (s *fakeStore) Get(context.Context, domain.Scope, string) (domain.TemporalFact, error) {
	return domain.TemporalFact{}, nil
}

func (s *fakeStore) List(context.Context, domain.Scope, port.ListQuery) ([]domain.TemporalFact, error) {
	return nil, nil
}

func (s *fakeStore) FindByMergeKey(context.Context, domain.Scope, string) ([]domain.TemporalFact, error) {
	return nil, nil
}

func (s *fakeStore) FindByRevisionSource(context.Context, domain.Scope, string) ([]domain.TemporalFact, error) {
	return nil, nil
}

func (s *fakeStore) FindSupersededBy(context.Context, domain.Scope, string) ([]domain.TemporalFact, error) {
	return nil, nil
}

func (s *fakeStore) UpdateValidity(_ context.Context, _ domain.Scope, factID string, _ time.Time, _ string) error {
	if s.updateValid == nil {
		return nil
	}
	return s.updateValid[factID]
}

func (s *fakeStore) ReopenValidity(_ context.Context, _ domain.Scope, factID string, _ string) error {
	s.reopened = append(s.reopened, factID)
	return s.reopenErr
}

func (s *fakeStore) Delete(_ context.Context, _ domain.Scope, ids []string) error {
	s.deleted = append(s.deleted, ids...)
	return s.deleteErr
}

func (s *fakeStore) UpdateFeedback(context.Context, domain.Scope, string, float64, float64) error {
	return nil
}

func (s *fakeStore) MarkClosed(context.Context, domain.Scope, string, bool) error { return nil }

func (s *fakeStore) ListByID(context.Context, domain.Scope, string) ([]domain.TemporalFact, error) {
	return nil, nil
}

func (s *fakeStore) DeleteByScope(context.Context, domain.Scope) (int, error) { return 0, nil }

func (s *fakeStore) Close() error { return nil }

type recordHook struct {
	events []diagnostic.StageDiagnostic
}

func (h *recordHook) OnStage(d diagnostic.StageDiagnostic) { h.events = append(h.events, d) }

func TestAppend_HappyPathStoresIDs(t *testing.T) {
	store := &fakeStore{}
	s := stages.NewAppend(store, nil)
	state := &write.WriteState{
		Resolution: domain.Resolution{Facts: []domain.TemporalFact{{ID: "a"}, {ID: "b"}}},
		Trace:      &domain.SaveTrace{},
	}
	d, err := s.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := d.(diagnostic.AppendDetail).Facts; got != 2 {
		t.Errorf("Detail.Facts = %d", got)
	}
	if got := state.AppendedFactIDs; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("ids = %v", got)
	}
}

func TestAppend_StoreErrPropagates(t *testing.T) {
	boom := errors.New("store down")
	s := stages.NewAppend(&fakeStore{appendErr: boom}, nil)
	state := &write.WriteState{Resolution: domain.Resolution{Facts: []domain.TemporalFact{{ID: "a"}}}}
	_, err := s.Run(context.Background(), state)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if state.FailedStage != "append" {
		t.Errorf("FailedStage = %q", state.FailedStage)
	}
}

func TestAppend_CompensateDeletesAppendedFacts(t *testing.T) {
	store := &fakeStore{}
	hook := &recordHook{}
	s := stages.NewAppend(store, hook)
	state := &write.WriteState{
		Scope:           domain.Scope{RuntimeID: "rt"},
		AppendedFactIDs: []string{"a", "b"},
	}
	if err := s.Compensate(context.Background(), state); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if got := store.deleted; len(got) != 2 || got[0] != "a" {
		t.Errorf("deleted = %v", got)
	}
}

func TestAppend_CompensateReopensWhenValidityCloseFailed(t *testing.T) {
	store := &fakeStore{deleteErr: errors.New("delete boom")}
	s := stages.NewAppend(store, nil)
	state := &write.WriteState{
		Scope:           domain.Scope{RuntimeID: "rt"},
		AppendedFactIDs: []string{"a"},
		AppliedCloses:   []domain.ValidityClose{{FactID: "prior", CorrectedBy: "a"}},
		FailedStage:     "validity_close",
	}
	_ = s.Compensate(context.Background(), state)
	if len(store.reopened) != 1 || store.reopened[0] != "prior" {
		t.Errorf("reopened = %v", store.reopened)
	}
}

// TestAppend_CompensateEmitsTelemetryOnStoreDeleteFailure pins the
// fix for the silent rollback bug. Before the patch, store.Delete
// failures inside Compensate were `_ = err`'d — operators had no way
// to learn that the ledger was now half-rolled-back. With telemetry
// emit in place, every failed cleanup leg surfaces a
// CompensationFailedDetail through the registered hook.
func TestAppend_CompensateEmitsTelemetryOnStoreDeleteFailure(t *testing.T) {
	boom := errors.New("store delete unavailable")
	store := &fakeStore{deleteErr: boom}
	hook := &recordHook{}
	s := stages.NewAppend(store, hook)
	state := &write.WriteState{
		Scope:           domain.Scope{RuntimeID: "rt"},
		AppendedFactIDs: []string{"a"},
		FailedStage:     "project_required",
	}
	if err := s.Compensate(context.Background(), state); err != nil {
		t.Fatalf("Compensate (best-effort) must not return err: %v", err)
	}
	if len(hook.events) != 1 {
		t.Fatalf("hook events = %d, want 1", len(hook.events))
	}
	ev := hook.events[0]
	if ev.Status != diagnostic.StatusFailed {
		t.Errorf("Status = %q, want failed", ev.Status)
	}
	d, ok := ev.Detail.(diagnostic.CompensationFailedDetail)
	if !ok {
		t.Fatalf("Detail type = %T", ev.Detail)
	}
	if d.OriginalStage != "save_rollback.store_delete" {
		t.Errorf("OriginalStage = %q", d.OriginalStage)
	}
	if d.Cause != "project_required" {
		t.Errorf("Cause = %q", d.Cause)
	}
}

func TestAppend_CompensateEmitsTelemetryOnReopenFailure(t *testing.T) {
	store := &fakeStore{reopenErr: errors.New("reopen conflict")}
	hook := &recordHook{}
	s := stages.NewAppend(store, hook)
	state := &write.WriteState{
		Scope:           domain.Scope{RuntimeID: "rt"},
		AppendedFactIDs: []string{"a"},
		AppliedCloses: []domain.ValidityClose{
			{FactID: "prior-1", CorrectedBy: "a"},
			{FactID: "prior-2", CorrectedBy: "a"},
		},
		FailedStage: "validity_close",
	}
	if err := s.Compensate(context.Background(), state); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	// Both reopen attempts ran (loop must not abort on first error)
	// and each emitted a distinct CompensationFailedDetail.
	if len(store.reopened) != 2 {
		t.Errorf("reopen attempts = %d, want 2", len(store.reopened))
	}
	if len(hook.events) != 2 {
		t.Fatalf("hook events = %d, want 2", len(hook.events))
	}
	for i, ev := range hook.events {
		d, ok := ev.Detail.(diagnostic.CompensationFailedDetail)
		if !ok {
			t.Fatalf("event %d Detail = %T", i, ev.Detail)
		}
		wantPrefix := "save_rollback.reopen_validity:prior-"
		if d.OriginalStage != wantPrefix+"1" && d.OriginalStage != wantPrefix+"2" {
			t.Errorf("event %d OriginalStage = %q", i, d.OriginalStage)
		}
	}
}
