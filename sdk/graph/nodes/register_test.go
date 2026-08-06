package nodes

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
)

func TestRegisterHelpers(t *testing.T) {
	reg := graph.NewRegistry()

	if err := RegisterInference(reg, InferenceNodeDeps{}); err != nil {
		t.Fatalf("RegisterInference: %v", err)
	}
	if err := RegisterTool(reg, nil); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	for _, name := range []string{"inference", "tool"} {
		if !reg.Has(name) {
			t.Fatalf("type %q not registered", name)
		}
	}

	// Duplicate registration must fail rather than silently replace.
	if err := RegisterTool(reg, nil); err == nil {
		t.Fatal("duplicate RegisterTool succeeded")
	}
}
