package stages_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write/stages"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

func TestSourceEvidenceSpansForJobRequiresCanonicalSpans(t *testing.T) {
	job := port.AsyncSemanticJob{
		TurnsSnapshot: []domain.TurnContext{{ID: "turn-stable", Text: "snapshot only"}},
	}
	if _, err := stages.SourceEvidenceSpansForJob(context.Background(), episodeTurnObservationStore{}, domain.Scope{}, job); err == nil {
		t.Fatal("SourceEvidenceSpansForJob err = nil, want canonical source evidence error")
	}
}

func TestSourceEvidenceSpansForJobRevalidatesCanonicalSpans(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "user"}
	observations := episodeTurnObservationStore{byID: map[string]domain.Observation{"obs-1": {
		ID:       "obs-1",
		Scope:    scope,
		Kind:     domain.ObservationKindTurn,
		SourceID: "turn-1",
		Text:     "temperature = 0.2",
		Spans: []domain.ObservationSpan{{
			ID:            "span-1",
			ObservationID: "obs-1",
			SourceID:      "turn-1",
			Text:          "temperature = 0.2",
			Start:         0,
			End:           len("temperature = 0.2"),
		}},
	}}}
	job := port.AsyncSemanticJob{SourceEvidenceSpans: []domain.SourceEvidenceSpan{{
		ObservationID: "obs-1",
		SpanID:        "span-1",
		SourceID:      "turn-1",
		Text:          "stale enqueue text",
	}}}
	spans, err := stages.SourceEvidenceSpansForJob(context.Background(), observations, scope, job)
	if err != nil {
		t.Fatalf("SourceEvidenceSpansForJob: %v", err)
	}
	if spans[0].Text != "temperature = 0.2" {
		t.Fatalf("span text = %q, want canonical store text", spans[0].Text)
	}
}

type episodeTurnObservationStore struct {
	byID map[string]domain.Observation
}

func (s episodeTurnObservationStore) Append(context.Context, []domain.Observation) error { return nil }
func (s episodeTurnObservationStore) Get(_ context.Context, _ domain.Scope, id string) (domain.Observation, error) {
	obs, ok := s.byID[id]
	if !ok {
		return domain.Observation{}, fmt.Errorf("not found")
	}
	return obs, nil
}
func (s episodeTurnObservationStore) List(context.Context, domain.Scope, port.ObservationListQuery) ([]domain.Observation, error) {
	return nil, nil
}
func (s episodeTurnObservationStore) Delete(context.Context, domain.Scope, []string) error {
	return nil
}
func (s episodeTurnObservationStore) DeleteByScope(context.Context, domain.Scope) (int, error) {
	return 0, nil
}
func (s episodeTurnObservationStore) Close() error { return nil }
