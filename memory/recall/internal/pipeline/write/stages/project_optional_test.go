package stages_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write/stages"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

func TestProjectOptional_RunsBestEffort(t *testing.T) {
	p := &stubProj{name: "optional", consistency: port.Optional}
	fanout := pipeline.NewFanout([]port.Projection{p}, nil)
	s := stages.NewProjectOptional(fanout)
	state := &write.WriteState{
		Resolution: domain.Resolution{Facts: []domain.TemporalFact{{ID: "a"}}},
	}
	d, err := s.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, ok := d.(diagnostic.ProjectDetail); !ok || got.Consistency != "optional" {
		t.Errorf("Detail = %#v", d)
	}
}

func TestProjectOptional_SkipsWhenNoWork(t *testing.T) {
	fanout := pipeline.NewFanout(nil, nil)
	s := stages.NewProjectOptional(fanout)
	state := &write.WriteState{Resolution: domain.Resolution{}}
	skip, det := s.Skip(context.Background(), state)
	if !skip {
		t.Fatal("Skip should fire on empty resolution")
	}
	if _, ok := det.(diagnostic.ProjectDetail); !ok {
		t.Errorf("skip Detail = %#v", det)
	}
}
