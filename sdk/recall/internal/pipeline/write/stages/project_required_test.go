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

type stubProj struct {
	name        string
	consistency port.Consistency
	projectErr  error
	forgetN     int
}

func (s *stubProj) Name() string                  { return s.name }
func (s *stubProj) Consistency() port.Consistency { return s.consistency }
func (s *stubProj) Project(context.Context, []domain.TemporalFact) error {
	return s.projectErr
}
func (s *stubProj) Forget(context.Context, domain.Scope, []string) error {
	s.forgetN++
	return nil
}
func (s *stubProj) Rebuild(context.Context, domain.Scope, []domain.TemporalFact) error {
	return nil
}

func TestProjectRequired_HappyPath(t *testing.T) {
	p := &stubProj{name: "required", consistency: port.Required}
	fanout := pipeline.NewFanout([]port.Projection{p}, nil)
	s := stages.NewProjectRequired(fanout, nil)
	state := &write.WriteState{
		Scope:           domain.Scope{RuntimeID: "rt"},
		Resolution:      domain.Resolution{Facts: []domain.TemporalFact{{ID: "a"}}},
		AppendedFactIDs: []string{"a"},
	}
	if _, err := s.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.RequiredApplied != 1 {
		t.Errorf("RequiredApplied = %d", state.RequiredApplied)
	}
}

func TestProjectRequired_FailureRunsSelfCleanup(t *testing.T) {
	p := &stubProj{name: "required", consistency: port.Required, projectErr: errors.New("project boom")}
	fanout := pipeline.NewFanout([]port.Projection{p}, nil)
	hook := &recordHook{}
	s := stages.NewProjectRequired(fanout, hook)
	state := &write.WriteState{
		Scope:           domain.Scope{RuntimeID: "rt"},
		Resolution:      domain.Resolution{Facts: []domain.TemporalFact{{ID: "a"}}},
		AppendedFactIDs: []string{"a"},
	}
	_, err := s.Run(context.Background(), state)
	if err == nil {
		t.Fatal("Run should propagate err")
	}
	if state.FailedStage != "project_required" {
		t.Errorf("FailedStage = %q", state.FailedStage)
	}
	if p.forgetN == 0 {
		t.Error("self-cleanup should call fanout.ForgetRequired")
	}
	_ = diagnostic.ProjectDetail{}
}

func TestProjectRequired_CompensateForgets(t *testing.T) {
	p := &stubProj{name: "required", consistency: port.Required}
	fanout := pipeline.NewFanout([]port.Projection{p}, nil)
	s := stages.NewProjectRequired(fanout, nil)
	state := &write.WriteState{
		Scope:           domain.Scope{RuntimeID: "rt"},
		AppendedFactIDs: []string{"a"},
	}
	if err := s.Compensate(context.Background(), state); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if p.forgetN != 1 {
		t.Errorf("forget count = %d", p.forgetN)
	}
}
