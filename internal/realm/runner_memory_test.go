package realm

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/event"
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

func TestEventBusFromReq_NilExtensions(t *testing.T) {
	req := &workflow.Request{}
	if eventBusFromReq(req) != nil {
		t.Fatal("expected nil for nil extensions")
	}
}

func TestEventBusFromReq_WithBus(t *testing.T) {
	bus := event.NewMemoryBus()
	req := &workflow.Request{
		Extensions: map[string]any{"event_bus": bus},
	}
	got := eventBusFromReq(req)
	if got == nil {
		t.Fatal("expected non-nil bus")
	}
}

func TestEventBusFromReq_WrongType(t *testing.T) {
	req := &workflow.Request{
		Extensions: map[string]any{"event_bus": "not-a-bus"},
	}
	if eventBusFromReq(req) != nil {
		t.Fatal("expected nil for wrong type")
	}
}

func TestOnStartFromReq_NilExtensions(t *testing.T) {
	req := &workflow.Request{}
	if onStartFromReq(req) != nil {
		t.Fatal("expected nil for nil extensions")
	}
}

func TestOnStartFromReq_WithCallback(t *testing.T) {
	called := false
	req := &workflow.Request{
		Extensions: map[string]any{"on_start": func() { called = true }},
	}
	fn := onStartFromReq(req)
	if fn == nil {
		t.Fatal("expected non-nil callback")
	}
	fn()
	if !called {
		t.Fatal("callback should have been called")
	}
}

func TestOnStartFromReq_WrongType(t *testing.T) {
	req := &workflow.Request{
		Extensions: map[string]any{"on_start": 42},
	}
	if onStartFromReq(req) != nil {
		t.Fatal("expected nil for wrong type")
	}
}

func TestDefaultParallelConfig(t *testing.T) {
	cfg := defaultParallelConfig()
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.Enabled == nil || !*cfg.Enabled {
		t.Fatal("expected Enabled=true")
	}
	if cfg.MaxBranches != 10 {
		t.Fatalf("expected MaxBranches=10, got %d", cfg.MaxBranches)
	}
	if cfg.MaxNesting != 3 {
		t.Fatalf("expected MaxNesting=3, got %d", cfg.MaxNesting)
	}
	if cfg.MergeStrategy != "last_wins" {
		t.Fatalf("expected MergeStrategy=last_wins, got %q", cfg.MergeStrategy)
	}
}

func TestMergeParallelConfig(t *testing.T) {
	base := defaultParallelConfig()
	disabled := false
	override := &model.ParallelConfig{
		Enabled:       &disabled,
		MaxBranches:   20,
		MergeStrategy: "first_wins",
	}
	merged := mergeParallelConfig(base, override)
	if merged.Enabled == nil || *merged.Enabled != false {
		t.Fatal("expected Enabled=false after merge")
	}
	if merged.MaxBranches != 20 {
		t.Fatalf("expected MaxBranches=20, got %d", merged.MaxBranches)
	}
	if merged.MaxNesting != 3 {
		t.Fatalf("expected MaxNesting=3 (not overridden), got %d", merged.MaxNesting)
	}
	if merged.MergeStrategy != "first_wins" {
		t.Fatalf("expected MergeStrategy=first_wins, got %q", merged.MergeStrategy)
	}
}

func TestMergeParallelConfig_EmptyOverride(t *testing.T) {
	base := defaultParallelConfig()
	override := &model.ParallelConfig{}
	merged := mergeParallelConfig(base, override)
	if *merged.Enabled != true {
		t.Fatal("expected Enabled unchanged")
	}
	if merged.MaxBranches != 10 {
		t.Fatal("expected MaxBranches unchanged")
	}
}

func TestEnvVarsMap(t *testing.T) {
	t.Setenv("FLOWCRAFT_TEST_VAR", "hello")
	m := envVarsMap()
	if m["FLOWCRAFT_TEST_VAR"] != "hello" {
		t.Fatalf("expected FLOWCRAFT_TEST_VAR=hello, got %v", m["FLOWCRAFT_TEST_VAR"])
	}
}
