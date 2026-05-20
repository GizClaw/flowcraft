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
	port.TelemetryHook
	projections []port.ProjectionEvent
}

func (h *recordHook) OnProjection(ev port.ProjectionEvent) { h.projections = append(h.projections, ev) }
func (h *recordHook) OnDrift(port.DriftEvent)              {}
func (h *recordHook) OnPipeline(port.PipelineEvent)        {}
func (h *recordHook) OnStage(diagnostic.StageDiagnostic)   {}

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
	if len(state.Trace.Appended) != 2 {
		t.Errorf("Trace.Appended not populated")
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

func TestAppend_CompensateEmitsSaveRollbackStoreDeleteOnErr(t *testing.T) {
	hook := &recordHook{}
	s := stages.NewAppend(&fakeStore{deleteErr: errors.New("boom")}, hook)
	state := &write.WriteState{
		Scope:           domain.Scope{RuntimeID: "rt"},
		AppendedFactIDs: []string{"a"},
		FailedStage:     "project_required",
	}
	_ = s.Compensate(context.Background(), state)
	if len(hook.projections) != 1 || hook.projections[0].Projection != "save_rollback.store_delete" {
		t.Errorf("emit = %+v, want save_rollback.store_delete", hook.projections)
	}
}

func TestAppend_CompensateEmitsAppendedFactsAndReopensWhenValidityCloseFailed(t *testing.T) {
	hook := &recordHook{}
	store := &fakeStore{
		deleteErr: errors.New("delete boom"),
		reopenErr: errors.New("reopen boom"),
	}
	s := stages.NewAppend(store, hook)
	state := &write.WriteState{
		Scope:           domain.Scope{RuntimeID: "rt"},
		AppendedFactIDs: []string{"a"},
		AppliedCloses:   []domain.ValidityClose{{FactID: "prior", CorrectedBy: "a"}},
		FailedStage:     "validity_close",
	}
	_ = s.Compensate(context.Background(), state)
	if len(hook.projections) != 2 {
		t.Fatalf("want 2 emits, got %d: %+v", len(hook.projections), hook.projections)
	}
	if hook.projections[0].Projection != "save_rollback.appended_facts" {
		t.Errorf("emit[0] = %q", hook.projections[0].Projection)
	}
	if hook.projections[1].Projection != "save_rollback.reopen_validity" {
		t.Errorf("emit[1] = %q", hook.projections[1].Projection)
	}
}
