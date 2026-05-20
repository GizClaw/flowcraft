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

type stubResolver struct {
	res domain.Resolution
	err error
}

func (s stubResolver) ResolveConflicts(_ context.Context, _ port.View, _ []domain.TemporalFact) (domain.Resolution, error) {
	return s.res, s.err
}

// stubResolveStore exists because Resolve.Run constructs an
// ingest.StoreView from the supplied port.TemporalStore method
// values — passing a nil store would nil-panic before the resolver
// even runs. The methods themselves are never invoked by the stub
// resolver, so they panic if the contract is ever broken in test.
type stubResolveStore struct{ port.TemporalStore }

func newStubResolveStore() port.TemporalStore { return stubResolveStore{} }

func TestResolve_PopulatesResolution(t *testing.T) {
	facts := []domain.TemporalFact{{ID: "a"}, {ID: "b"}}
	closes := []domain.ValidityClose{{FactID: "prior", CorrectedBy: "a"}}
	s := stages.NewResolve(stubResolver{res: domain.Resolution{Facts: facts, Closes: closes}}, newStubResolveStore())
	state := &write.WriteState{Ingest: port.IngestResult{Facts: facts}}

	d, err := s.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	got, ok := d.(diagnostic.ResolveDetail)
	if !ok {
		t.Fatalf("Detail type %T", d)
	}
	if got.Appended != 2 || got.Closed != 1 {
		t.Errorf("Detail = %+v", got)
	}
	if len(state.Resolution.Facts) != 2 || len(state.Resolution.Closes) != 1 {
		t.Errorf("Resolution not set: %+v", state.Resolution)
	}
}

func TestResolve_EmptyShortCircuits(t *testing.T) {
	s := stages.NewResolve(stubResolver{res: domain.Resolution{}}, newStubResolveStore())
	state := &write.WriteState{}
	_, err := s.Run(context.Background(), state)
	var sc pipeline.ShortCircuit
	if !errors.As(err, &sc) {
		t.Fatalf("want ShortCircuit, got %v", err)
	}
}

func TestResolve_NilResolverSkips(t *testing.T) {
	facts := []domain.TemporalFact{{ID: "a"}}
	s := stages.NewResolve(nil, nil)
	state := &write.WriteState{Ingest: port.IngestResult{Facts: facts}}
	skip, det := s.Skip(context.Background(), state)
	if !skip {
		t.Fatal("nil resolver must skip")
	}
	got, ok := det.(diagnostic.ResolveDetail)
	if !ok || got.Appended != 1 {
		t.Errorf("skip detail = %#v", det)
	}
	if len(state.Resolution.Facts) != 1 {
		t.Errorf("Resolution should mirror Ingest.Facts when skipping, got %+v", state.Resolution)
	}
}

func TestResolve_ErrPropagates(t *testing.T) {
	boom := errors.New("resolver down")
	s := stages.NewResolve(stubResolver{err: boom}, newStubResolveStore())
	state := &write.WriteState{Ingest: port.IngestResult{Facts: []domain.TemporalFact{{ID: "a"}}}}
	_, err := s.Run(context.Background(), state)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v want %v", err, boom)
	}
	if state.FailedStage != "resolve" {
		t.Errorf("FailedStage = %q", state.FailedStage)
	}
}
