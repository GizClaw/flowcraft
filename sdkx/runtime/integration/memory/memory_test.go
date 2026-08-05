package memory

import (
	"context"
	"reflect"
	"testing"
	"time"

	rootmemory "github.com/GizClaw/flowcraft/memory"
	"github.com/GizClaw/flowcraft/memory/sources"
	"github.com/GizClaw/flowcraft/memory/worker"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	memoryconfig "github.com/GizClaw/flowcraft/sdkx/memory/config"
)

func TestFactorySpecRequiresOnlyAssembly(t *testing.T) {
	spec := NewFactory().Spec()
	if spec.Kind != Kind || len(spec.Deps) != 1 {
		t.Fatalf("Spec = %+v", spec)
	}
	dep := spec.Deps[0]
	if dep.Name != "memory" || dep.Kind != "memory.Assembly" || !dep.Required ||
		dep.Type != reflect.TypeFor[*memoryconfig.Assembly]() {
		t.Fatalf("dependency = %+v", dep)
	}
}

func TestStartCloseIdempotentDoesNotCloseAssembly(t *testing.T) {
	catalog, err := sources.NewWorkspaceScopeCatalog(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	runner, err := worker.NewRunner(worker.RunnerConfig{
		Processor: &worker.Processor{},
		Catalog:   catalog,
		Scopes:    []sdkmemory.Scope{{RuntimeID: "memory"}},
		Interval:  time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	system := &rootmemory.System{}
	assembly := &memoryconfig.Assembly{System: system, Runner: runner}
	integration := &integration{name: "memory", runner: runner, bound: true}
	if err := integration.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := integration.Start(context.Background()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if err := integration.Close(); err != nil {
		t.Fatal(err)
	}
	if err := integration.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if assembly.System != system {
		t.Fatal("integration mutated or closed the borrowed Assembly")
	}
	if err := assembly.Close(); err != nil {
		t.Fatalf("owner Close after integration Close: %v", err)
	}
}

func TestRollbackBeforeBindIsSafe(t *testing.T) {
	integration := &integration{name: "memory"}
	if err := integration.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := integration.Close(); err != nil {
		t.Fatal(err)
	}
}
