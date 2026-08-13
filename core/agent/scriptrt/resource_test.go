package scriptrt

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/resource"
)

func TestRegisterAddsJSRuntimeFactory(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, impl := range []string{"js", "lua"} {
		if _, ok := reg.Lookup(ResourceKind, impl); !ok {
			t.Fatalf("factory %s/%s missing after Register", ResourceKind, impl)
		}
	}
}
