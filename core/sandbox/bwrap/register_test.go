package bwrap

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/resource"
)

func TestRegisterAddsBwrapFactory(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Lookup(ResourceKind, BackendName); !ok {
		t.Fatalf("factory %s/%s missing", ResourceKind, BackendName)
	}
}

func TestNewRequiresRoot(t *testing.T) {
	if _, err := NewFactory().New(context.Background(), resource.NewInput(json.RawMessage(`{"root": ""}`), nil, nil, nil)); err == nil {
		t.Fatal("New accepted missing root")
	}
}
