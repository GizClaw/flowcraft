package config_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/inference/inferencetest"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	memoryconfig "github.com/GizClaw/flowcraft/sdk/memory/config"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

type fakeAssembly struct{}

func (fakeAssembly) Context(context.Context, sdkmemory.ContextRequest) (sdkmemory.ContextResult, error) {
	return sdkmemory.ContextResult{}, nil
}
func (fakeAssembly) CommitTurn(context.Context, sdkmemory.Turn) error { return nil }
func (fakeAssembly) PutDocument(context.Context, sdkmemory.Document) error {
	return nil
}

var _ sdkmemory.Assembly = fakeAssembly{}

func TestDeployFactorySpec(t *testing.T) {
	got := memoryconfig.NewDeployFactory(
		"flowcraft",
		nil,
		sdkconfig.ResourceDepSpec{Name: "inference", Type: "inference.Runtime", Required: true},
		sdkconfig.ResourceDepSpec{Name: "workspace", Type: "workspace.Workspace", Required: true},
	).Spec()
	want := sdkconfig.ResourceSpec{
		Kind:     memoryconfig.ResourceKind,
		Impl:     "flowcraft",
		ItemType: "memory.System",
		Deps: []sdkconfig.ResourceDepSpec{
			{Name: "inference", Type: "inference.Runtime", Required: true},
			{Name: "workspace", Type: "workspace.Workspace", Required: true},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
}

func TestDeployFactoryNewPassesDepsAndSettings(t *testing.T) {
	var received sdkmemory.Input
	factory := memoryconfig.NewDeployFactory("flowcraft", sdkmemory.FactoryFunc(
		func(_ context.Context, input sdkmemory.Input) (sdkmemory.Assembly, error) {
			received = input
			return fakeAssembly{}, nil
		},
	))
	ws := workspace.NewMemWorkspace()
	runtime := (&inferencetest.GenerateFake{}).Runtime(t)
	value, err := factory.New(context.Background(), sdkconfig.Input{
		Resolve: resolveLiteral(t),
		Settings: literalSettings(t, `{
			"version": "v1",
			"chunk": {"max_runes": 1600}
		}`),
		Deps: map[string]any{
			"workspace": ws,
			"inference": runtime,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := value.(sdkmemory.Assembly); !ok {
		t.Fatalf("New returned %T, want memory.Assembly", value)
	}
	if received.Deps["workspace"] != ws || received.Deps["inference"] != runtime {
		t.Fatalf("factory received deps = %#v", received.Deps)
	}
	var decoded map[string]any
	if err := json.Unmarshal(received.Settings, &decoded); err != nil {
		t.Fatalf("factory received non-JSON settings %q: %v", received.Settings, err)
	}
	chunk, ok := decoded["chunk"].(map[string]any)
	if !ok || chunk["max_runes"] != float64(1600) {
		t.Fatalf("factory received settings = %q", received.Settings)
	}
}

func TestDeployFactoryNewRejectsInvalidInputs(t *testing.T) {
	factory := memoryconfig.NewDeployFactory("flowcraft", sdkmemory.FactoryFunc(
		func(context.Context, sdkmemory.Input) (sdkmemory.Assembly, error) {
			return fakeAssembly{}, nil
		},
	))
	if _, err := factory.New(context.Background(), sdkconfig.Input{
		Settings: settingsJSON(t, `{"unknown": true}`),
	}); err == nil {
		t.Fatal("New accepted an unknown resource setting")
	}
	if _, err := memoryconfig.NewDeployFactory("flowcraft", nil).New(
		context.Background(),
		sdkconfig.Input{},
	); err == nil {
		t.Fatal("New with nil implementation factory succeeded")
	}
}

func settingsJSON(t *testing.T, raw string) *sdkconfig.Opaque {
	t.Helper()
	var opaque sdkconfig.Opaque
	if err := json.Unmarshal([]byte(raw), &opaque); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return &opaque
}

func literalSettings(t *testing.T, doc string) *sdkconfig.Opaque {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal literal settings: %v", err)
	}
	var opaque sdkconfig.Opaque
	if err := json.Unmarshal(raw, &opaque); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return &opaque
}

func resolveLiteral(t *testing.T) func(context.Context, sdkconfig.Source) ([]byte, error) {
	t.Helper()
	return func(ctx context.Context, src sdkconfig.Source) ([]byte, error) {
		return sdkconfig.NewLoader().Load(ctx, src)
	}
}
