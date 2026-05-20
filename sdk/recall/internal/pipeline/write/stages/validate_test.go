package stages_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write/stages"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

func TestValidate_Accepts(t *testing.T) {
	s := stages.NewValidate()
	state := &write.WriteState{Scope: domain.Scope{RuntimeID: "rt"}, Turns: []port.TurnContext{{ID: "t1"}}}
	d, err := s.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	got, ok := d.(diagnostic.ValidateDetail)
	if !ok {
		t.Fatalf("Detail type = %T, want ValidateDetail", d)
	}
	if got.InputTurns != 1 || got.Rejected != 0 || got.RejectReason != "" {
		t.Errorf("Detail = %+v, want InputTurns=1 Rejected=0", got)
	}
	if state.FailedStage != "" {
		t.Errorf("FailedStage = %q, want empty on success", state.FailedStage)
	}
}

func TestValidate_RejectsMissingRuntimeID(t *testing.T) {
	s := stages.NewValidate()
	state := &write.WriteState{}
	d, err := s.Run(context.Background(), state)
	if err == nil {
		t.Fatal("expected error for empty RuntimeID")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("err class lost; want errdefs.Validation, got %v", err)
	}
	got, ok := d.(diagnostic.ValidateDetail)
	if !ok || got.Rejected != 1 || got.RejectReason == "" {
		t.Errorf("Detail = %+v, want Rejected=1 with RejectReason", d)
	}
	if state.FailedStage != "validate" {
		t.Errorf("FailedStage = %q, want validate", state.FailedStage)
	}
}
