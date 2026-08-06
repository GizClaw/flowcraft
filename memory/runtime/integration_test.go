package runtime

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/config"
	"github.com/GizClaw/flowcraft/memory/sources"
	"github.com/GizClaw/flowcraft/memory/worker"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestFactorySpecRequiresFlowcraftAssembly(t *testing.T) {
	spec := NewFactory().Spec()
	if spec.Kind != Kind || len(spec.Deps) != 1 {
		t.Fatalf("Spec = %+v", spec)
	}
	dep := spec.Deps[0]
	if dep.Name != "memory" || dep.Kind != "memory.Assembly" || !dep.Required ||
		dep.Type != reflect.TypeFor[*config.Assembly]() {
		t.Fatalf("dependency = %+v", dep)
	}
}

func TestStartCloseIdempotent(t *testing.T) {
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
	assembly := &config.Assembly{Runner: runner}
	integration := &integration{name: "memory", assembly: assembly, bound: true}
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
