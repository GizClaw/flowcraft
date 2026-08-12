package sandbox_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

func TestRegister(t *testing.T) {
	reg := resource.NewRegistry()
	if err := sandbox.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	factory, ok := reg.Lookup("sandbox.Runner", "local")
	if !ok {
		t.Fatal("sandbox.Runner/local factory not registered")
	}
	value, err := factory.New(context.Background(), resource.Input{
		Settings: []byte(`{"root": "` + t.TempDir() + `"}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := value.(*sandbox.LocalRunner); !ok {
		t.Fatalf("New returned %T, want *sandbox.LocalRunner", value)
	}
}

func TestFactoryRequiresRoot(t *testing.T) {
	reg := resource.NewRegistry()
	if err := sandbox.Register(reg); err != nil {
		t.Fatal(err)
	}
	factory, _ := reg.Lookup("sandbox.Runner", "local")
	if _, err := factory.New(context.Background(), resource.Input{}); err == nil {
		t.Fatal("New unexpectedly accepted missing root")
	}
}
