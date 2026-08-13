package local_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/sandbox/local"
)

func TestRegister(t *testing.T) {
	reg := resource.NewRegistry()
	if err := local.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	factory, ok := reg.Lookup(local.ResourceKind, local.BackendName)
	if !ok {
		t.Fatal("sandbox.Runner/local factory not registered")
	}
	value, err := factory.New(context.Background(), resource.Input{
		Settings: []byte(`{"root": "` + t.TempDir() + `"}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := value.(*local.Runner); !ok {
		t.Fatalf("New returned %T, want *local.Runner", value)
	}
}

func TestFactoryRequiresRoot(t *testing.T) {
	reg := resource.NewRegistry()
	if err := local.Register(reg); err != nil {
		t.Fatal(err)
	}
	factory, _ := reg.Lookup(local.ResourceKind, local.BackendName)
	if _, err := factory.New(context.Background(), resource.Input{}); err == nil {
		t.Fatal("New unexpectedly accepted missing root")
	}
}
