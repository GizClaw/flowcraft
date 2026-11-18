package host

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy/builtin"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// An operator overriding a description has to be able to read the wording a
// deployment currently prompts with, which is the assembled text — not the string
// a built-in tool would have produced.
func TestManagerExposesTheModelFacingToolDefinitions(t *testing.T) {
	manager := newManager(t, builtin.Kinds().WithEngines(engineFor("echo", func() agent.Runner {
		return &textRunner{text: "hello"}
	})), testHost(t))
	spec := testSpec(t, "a", "echo")
	spec.Runtime.Tools.Descriptions = runtime.DescriptionsConfig{
		"read_file": {Text: "Read a file; paths are absolute."},
	}
	if err := manager.Add(context.Background(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}

	definitions, ok := manager.ToolDefinitions("a")
	if !ok {
		t.Fatal("ToolDefinitions(a) = false")
	}
	seen := false
	for _, definition := range definitions {
		if definition.Name != "read_file" {
			continue
		}
		seen = true
		if definition.Description != "Read a file; paths are absolute." {
			t.Errorf("read_file description = %q, want the configured text", definition.Description)
		}
	}
	if !seen {
		t.Fatalf("read_file is not in %d definitions", len(definitions))
	}
	if _, ok := manager.ToolDefinitions("absent"); ok {
		t.Error("ToolDefinitions on an unknown agent succeeded")
	}
}
