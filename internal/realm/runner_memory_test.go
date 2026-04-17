package realm

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

func TestRuntimeIDFor_PrefersContextOverInput(t *testing.T) {
	ctx := model.WithRuntimeID(context.Background(), "from-ctx")
	req := &workflow.Request{RuntimeID: "from-input"}
	if got := runtimeIDFor(ctx, req); got != "from-ctx" {
		t.Fatalf("got %q", got)
	}
}

func TestRuntimeIDFor_FallsBackToInput(t *testing.T) {
	ctx := context.Background()
	req := &workflow.Request{RuntimeID: "fallback-rt"}
	if got := runtimeIDFor(ctx, req); got != "fallback-rt" {
		t.Fatalf("got %q", got)
	}
}
