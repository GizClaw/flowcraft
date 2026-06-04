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
	projected  []domain.Observation
}

func (p *observationProjectionRecorder) Name() string { return "observation" }

func (p *observationProjectionRecorder) ProjectObservations(_ context.Context, observations []domain.Observation) error {
	p.projected = append(p.projected, observations...)
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

func TestCommitObservations_RejectsInvalidEvidenceWindowRefs(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	stored := domain.Observation{
		ID:        "obs-1",
		Scope:     scope,
		Kind:      domain.ObservationKindTurn,
		SourceID:  "src-1",
		SessionID: "session-1",
		Text:      "temperature 调成 0.2",
		Spans: []domain.ObservationSpan{{
			ID:            "span-1",
			ObservationID: "obs-1",
			SourceID:      "src-1",
			Kind:          domain.ObservationSpanKindSentence,
			Text:          "temperature 调成 0.2",
			Start:         0,
			End:           len("temperature 调成 0.2"),
		}},
	}
	outsideScope := stored
	outsideScope.Scope = domain.Scope{RuntimeID: "rt", UserID: "other"}
	syntheticEvidence := stored
	syntheticEvidence.Kind = domain.ObservationKindEvidence
	expired := stored
	expired.Metadata = map[string]any{"expired": true}
	hardDeleted := stored
	hardDeleted.Metadata = map[string]any{"hard_deleted": true}
	crossObservationSpan := stored
	crossObservationSpan.Spans = append([]domain.ObservationSpan(nil), stored.Spans...)
	crossObservationSpan.Spans[0].ObservationID = "obs-other"
	emptyObservationSpan := stored
	emptyObservationSpan.Spans = append([]domain.ObservationSpan(nil), stored.Spans...)
	emptyObservationSpan.Spans[0].ObservationID = ""

	cases := []struct {
		name    string
		getByID map[string]domain.Observation
		ref     domain.EvidenceWindowRef
	}{
		{
			name: "missing observation id",
			ref:  domain.EvidenceWindowRef{},
		},
		{
			name:    "observation not found",
			getByID: map[string]domain.Observation{},
			ref:     domain.EvidenceWindowRef{ObservationID: "missing"},
		},
		{
			name:    "outside scope",
			getByID: map[string]domain.Observation{"obs-1": outsideScope},
			ref:     domain.EvidenceWindowRef{ObservationID: "obs-1"},
		},
		{
			name:    "synthesized evidence observation",
			getByID: map[string]domain.Observation{"obs-1": syntheticEvidence},
			ref:     domain.EvidenceWindowRef{ObservationID: "obs-1"},
		},
		{
			name:    "expired observation",
			getByID: map[string]domain.Observation{"obs-1": expired},
			ref:     domain.EvidenceWindowRef{ObservationID: "obs-1"},
		},
		{
			name:    "hard deleted observation",
			getByID: map[string]domain.Observation{"obs-1": hardDeleted},
			ref:     domain.EvidenceWindowRef{ObservationID: "obs-1"},
		},
		{
			name:    "cross observation span",
			getByID: map[string]domain.Observation{"obs-1": crossObservationSpan},
			ref:     domain.EvidenceWindowRef{ObservationID: "obs-1"},
		},
		{
			name:    "empty span observation id",
			getByID: map[string]domain.Observation{"obs-1": emptyObservationSpan},
			ref:     domain.EvidenceWindowRef{ObservationID: "obs-1"},
		},
		{
			name:    "source mismatch",
			getByID: map[string]domain.Observation{"obs-1": stored},
			ref:     domain.EvidenceWindowRef{ObservationID: "obs-1", SourceID: "src-other"},
		},
		{
			name:    "session mismatch",
			getByID: map[string]domain.Observation{"obs-1": stored},
			ref:     domain.EvidenceWindowRef{ObservationID: "obs-1", SessionID: "session-other"},
		},
		{
			name:    "span missing",
			getByID: map[string]domain.Observation{"obs-1": stored},
			ref:     domain.EvidenceWindowRef{ObservationID: "obs-1", SpanID: "span-other"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			observations := &observationStoreRecorder{getByID: tc.getByID}
			stage := stages.NewCommitObservations(observations, nil)
			state := &write.WriteState{
				Scope:              scope,
				EvidenceWindowRefs: []domain.EvidenceWindowRef{tc.ref},
			}

			_, err := stage.Run(context.Background(), state)
			if err == nil {
				t.Fatal("Run err = nil, want validation error")
			}
			if len(observations.appended) != 0 {
				t.Fatalf("invalid evidence window appended observations: %+v", observations.appended)
			}
			if len(state.SourceEvidenceSpans) != 0 {
				t.Fatalf("SourceEvidenceSpans = %+v, want none", state.SourceEvidenceSpans)
			}
		})
	}
}
