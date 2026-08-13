package hook

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/resource"
)

// TestContextPreparer_PreparerContract runs the shared agent.Preparer
// conformance suite against the memory.context seed hook.
func TestContextPreparer_PreparerContract(t *testing.T) {
	assembly, _, _ := newFakeAssembly(t)
	input := resource.Input{
		Settings: settingsNode(t, `
query:
  literal: alpha
scope:
  runtime_id: memory
  user_id: tenant
output: recalled
`),
		Deps: map[string]any{depName: assembly},
	}
	if _, err := (ContextPreparer{}).New(context.Background(), input); err != nil {
		t.Fatalf("New: %v", err)
	}
	agenttest.PreparerSuite(t, func() agent.Preparer {
		p, _ := (ContextPreparer{}).New(context.Background(), input)
		return p.(agent.Preparer)
	})
}

// TestTurnCommitter_CommitterContract runs the shared agent.Committer
// conformance suite against the memory.turn finalizer.
func TestTurnCommitter_CommitterContract(t *testing.T) {
	assembly, _, _ := newFakeAssembly(t)
	input := resource.Input{
		Settings: settingsNode(t, `
scope:
  runtime_id: memory
  user_id: tenant
channel: main
`),
		Deps: map[string]any{depName: assembly},
	}
	if _, err := (TurnCommitter{}).New(context.Background(), input); err != nil {
		t.Fatalf("New: %v", err)
	}
	agenttest.CommitterSuite(t, func() agent.Committer {
		c, _ := (TurnCommitter{}).New(context.Background(), input)
		return c.(agent.Committer)
	})
}
