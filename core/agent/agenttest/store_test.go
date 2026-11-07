package agenttest_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
)

// The suite must pass against the reference implementation, or it is describing
// something no backend can satisfy.
func TestMemStoreConformance(t *testing.T) {
	agenttest.Store(t, func(t *testing.T) agent.Store {
		return agenttest.NewMemStore()
	})
}
