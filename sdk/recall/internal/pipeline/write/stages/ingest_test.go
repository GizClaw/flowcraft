package stages_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write/stages"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

type stubIngestor struct {
	res port.IngestResult
	err error
}

func (s stubIngestor) Compile(_ context.Context, _ port.IngestInput) (port.IngestResult, error) {
	return s.res, s.err
}

func TestIngest_PopulatesStateAndTrace(t *testing.T) {
	facts := []domain.TemporalFact{{ID: "a", Kind: domain.KindNote}}
	snap := []port.EntitySnapshot{{Canonical: "alice"}}
	s := stages.NewIngest(stubIngestor{res: port.IngestResult{
		Facts:                facts,
		StructurizerCoverage: diagnostic.StructurizerCoverage{TotalFactsSeen: 1},
	}}, func(domain.Scope) []port.EntitySnapshot { return snap })

	state := &write.WriteState{
		Scope: domain.Scope{RuntimeID: "rt"},
		Trace: &domain.SaveTrace{},
	}
	d, err := s.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	got, ok := d.(diagnostic.IngestDetail)
	if !ok || got.ExtractedFacts != 1 {
		t.Fatalf("Detail = %#v", d)
	}
	if len(state.KnownEntities) != 1 || state.KnownEntities[0].Canonical != "alice" {
		t.Errorf("KnownEntities = %+v", state.KnownEntities)
	}
	if got.KnownEntitiesSeen != 1 {
		t.Errorf("KnownEntitiesSeen = %d", got.KnownEntitiesSeen)
	}
}

func TestIngest_EmptyShortCircuits(t *testing.T) {
	s := stages.NewIngest(stubIngestor{res: port.IngestResult{}}, nil)
	state := &write.WriteState{Scope: domain.Scope{RuntimeID: "rt"}}
	_, err := s.Run(context.Background(), state)
	var sc pipeline.ShortCircuit
	if !errors.As(err, &sc) {
		t.Fatalf("want ShortCircuit, got %v", err)
	}
}

func TestIngest_ErrPropagates(t *testing.T) {
	boom := errors.New("extract down")
	s := stages.NewIngest(stubIngestor{err: boom}, nil)
	state := &write.WriteState{Scope: domain.Scope{RuntimeID: "rt"}}
	_, err := s.Run(context.Background(), state)
	if !errors.Is(err, boom) {
		t.Fatalf("Run err = %v, want %v", err, boom)
	}
	if state.FailedStage != "ingest" {
		t.Errorf("FailedStage = %q, want ingest", state.FailedStage)
	}
}
