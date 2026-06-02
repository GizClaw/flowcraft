package stages_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write/stages"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

type observationStoreRecorder struct {
	appended []domain.Observation
	deleted  []string
	getByID  map[string]domain.Observation
}

func (s *observationStoreRecorder) Append(_ context.Context, observations []domain.Observation) error {
	s.appended = append(s.appended, observations...)
	return nil
}

func (s *observationStoreRecorder) Get(_ context.Context, _ domain.Scope, id string) (domain.Observation, error) {
	if s.getByID != nil {
		if obs, ok := s.getByID[id]; ok {
			return obs, nil
		}
	}
	return domain.Observation{}, port.ErrNotFound
}

func (s *observationStoreRecorder) List(context.Context, domain.Scope, port.ObservationListQuery) ([]domain.Observation, error) {
	return nil, nil
}

func (s *observationStoreRecorder) Delete(_ context.Context, _ domain.Scope, ids []string) error {
	s.deleted = append(s.deleted, ids...)
	return nil
}

func (s *observationStoreRecorder) DeleteByScope(context.Context, domain.Scope) (int, error) {
	return 0, nil
}

func (s *observationStoreRecorder) Close() error { return nil }

type observationProjectionRecorder struct {
	projectErr error
	forgotten  []string
}

func (p *observationProjectionRecorder) Name() string { return "observation" }

func (p *observationProjectionRecorder) ProjectObservations(context.Context, []domain.Observation) error {
	return p.projectErr
}

func (p *observationProjectionRecorder) RebuildObservations(context.Context, domain.Scope, []domain.Observation) error {
	return nil
}

func (p *observationProjectionRecorder) ForgetObservations(_ context.Context, _ domain.Scope, ids []string) error {
	p.forgotten = append(p.forgotten, ids...)
	return nil
}

func (p *observationProjectionRecorder) ClearObservationScope(context.Context, domain.Scope) error {
	return nil
}

func TestCommitObservations_CleansStoreWhenProjectionFails(t *testing.T) {
	boom := errors.New("projection down")
	observations := &observationStoreRecorder{}
	projection := &observationProjectionRecorder{projectErr: boom}
	stage := stages.NewCommitObservations(observations, projection)
	state := &write.WriteState{
		Scope: domain.Scope{RuntimeID: "rt", UserID: "u1"},
		Turns: []port.TurnContext{{
			ID:   "turn-1",
			Text: "raw text that must not survive a failed projection",
		}},
	}

	_, err := stage.Run(context.Background(), state)
	if !errors.Is(err, boom) {
		t.Fatalf("Run err = %v, want %v", err, boom)
	}
	if len(observations.appended) != 1 {
		t.Fatalf("appended observations = %d, want 1", len(observations.appended))
	}
	if len(observations.deleted) != 1 || observations.deleted[0] != observations.appended[0].ID {
		t.Fatalf("deleted = %v, want appended observation id %q", observations.deleted, observations.appended[0].ID)
	}
	if len(projection.forgotten) != 1 || projection.forgotten[0] != observations.appended[0].ID {
		t.Fatalf("projection forgotten = %v, want appended observation id %q", projection.forgotten, observations.appended[0].ID)
	}
	if len(state.RawObservationIDs) != 0 {
		t.Fatalf("RawObservationIDs = %v, want empty after cleanup", state.RawObservationIDs)
	}
}
