package runtime

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/event"
)

// TestBaseHost_HostSuiteContract runs the shared agent.Host conformance
// suite against the runtime's production baseHost. Each subtest builds
// a fresh host on its own bus.
func TestBaseHost_HostSuiteContract(t *testing.T) {
	agenttest.HostSuite(t, func() agent.Host {
		bus := event.NewMemoryBus()
		t.Cleanup(func() { _ = bus.Close() })
		factory, err := newBaseHostFactory(bus)
		if err != nil {
			t.Fatalf("newBaseHostFactory: %v", err)
		}
		return mustBaseHost(t, factory)
	})
}
