package hook

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/resource"
)

// TestPrepareFactory_PreparerContract runs the shared agent.Preparer
// conformance suite against the media.attachments prepare hook.
func TestPrepareFactory_PreparerContract(t *testing.T) {
	if _, err := (prepareFactory{}).New(context.Background(), resource.Input{}); err != nil {
		t.Fatalf("New: %v", err)
	}
	agenttest.PreparerSuite(t, func() agent.Preparer {
		p, _ := (prepareFactory{}).New(context.Background(), resource.Input{})
		return p.(agent.Preparer)
	})
}
