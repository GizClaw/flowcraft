package stages_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write/stages"
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
	if state.EvolutionErr != nil {
		t.Errorf("EvolutionErr should be nil on success, got %v", state.EvolutionErr)
	}
}

func TestEvolutionAfterSave_FailureIsNonFatal(t *testing.T) {
	boom := errors.New("evo down")
	ev := &stubEvolution{saveErr: boom}
	s := stages.NewEvolutionAfterSave(ev)
	state := &write.WriteState{AppendedFactIDs: []string{"a"}}
	if _, err := s.Run(context.Background(), state); err != nil {
		t.Fatalf("Run must swallow err: %v", err)
	}
	if !errors.Is(state.EvolutionErr, boom) {
		t.Errorf("EvolutionErr = %v, want %v", state.EvolutionErr, boom)
	}
}

func TestEvolutionAfterSave_NilRunnerSkips(t *testing.T) {
	s := stages.NewEvolutionAfterSave(nil)
	skip, _ := s.Skip(context.Background(), &write.WriteState{})
	if !skip {
		t.Fatal("nil runner must skip")
	}
}
