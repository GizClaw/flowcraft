package a2a

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/resource"
)

func TestRegisterAddsA2AEngineFactory(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Lookup(ResourceKind, Kind); !ok {
		t.Fatalf("factory %s/%s missing", ResourceKind, Kind)
	}
}

func TestFactorySpec(t *testing.T) {
	factory := NewFactory()
	spec := factory.Spec()
	if spec.Kind != ResourceKind || spec.Impl != Kind {
		t.Fatalf("spec = %+v", spec)
	}
}
