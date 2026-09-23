package vector

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// newTestWorkspace returns an isolated local workspace for one test.
func newTestWorkspace(t *testing.T) workspace.Workspace {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

// newTestRuntime builds an inference assembly from provider definitions
// through the same resource factory deployments use.
func newTestRuntime(t *testing.T, providers []inference.ProviderDefinition) *inference.Assembly {
	t.Helper()
	deps := make(map[string]any, len(providers))
	for index, provider := range providers {
		deps[fmt.Sprintf("provider.%d", index)] = provider
	}
	value, err := inference.Factory{}.New(context.Background(), resource.Input{Deps: deps})
	if err != nil {
		t.Fatal(err)
	}
	assembly, ok := value.(*inference.Assembly)
	if !ok {
		t.Fatalf("inference factory returned %T", value)
	}
	return assembly
}
