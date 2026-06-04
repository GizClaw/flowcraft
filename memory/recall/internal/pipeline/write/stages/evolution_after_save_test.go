package stages_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write/stages"
)

type stubEvolution struct {
	saveErr error
	saveN   int
}

func (s *stubEvolution) AfterSave(_ context.Context, _ domain.Scope, _ []string) error {
	s.saveN++
	return s.saveErr
}
func (*stubEvolution) AfterRecall(_ context.Context, _ domain.Scope, _ domain.RecallTrace) error {
	return nil
}

func TestEvolutionAfterSave_HappyPath(t *testing.T) {
	ev := &stubEvolution{}
	s := stages.NewEvolutionAfterSave(ev)
	state := &write.WriteState{AppendedFactIDs: []string{"a"}}
	if _, err := s.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ev.saveN != 1 {
		t.Errorf("AfterSave call count = %d", ev.saveN)
	}
}

// TestEvolutionAfterSave_FailureIsBestEffort pins the AfterSave
// failure contract: failures are surfaced via the BestEffort wrapper
// so the framework emits Status=Degraded without aborting Save.
func TestEvolutionAfterSave_FailureIsBestEffort(t *testing.T) {
	boom := errors.New("evo down")
	ev := &stubEvolution{saveErr: boom}
	s := stages.NewEvolutionAfterSave(ev)
	state := &write.WriteState{AppendedFactIDs: []string{"a"}}
	_, err := s.Run(context.Background(), state)
	if err == nil {
		t.Fatalf("Run must return a BestEffort-wrapped err, got nil")
	}
	var bef pipeline.BestEffortFailure
	if !errors.As(err, &bef) {
		t.Fatalf("err must be a pipeline.BestEffortFailure, got %T (%v)", err, err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("err must wrap the original cause via Unwrap, got %v", err)
	}
}

func TestEvolutionAfterSave_NilRunnerSkips(t *testing.T) {
	s := stages.NewEvolutionAfterSave(nil)
	skip, _ := s.Skip(context.Background(), &write.WriteState{})
	if !skip {
		t.Fatal("nil runner must skip")
	}
}

func TestEvolutionAfterSave_AsyncNoAppendedFactsSkips(t *testing.T) {
	s := stages.NewEvolutionAfterSave(&stubEvolution{})
	state := &write.WriteState{Mode: domain.WriteModeAsyncSemantic}
	skip, _ := s.Skip(context.Background(), state)
	if !skip {
		t.Fatal("async save with no appended semantic facts must skip")
	}
}
