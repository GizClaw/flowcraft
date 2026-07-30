package agenttest_test

// Self-test for HostSuite: NoopHost is the canonical zero-value
// host implementation, so it should pass every contract subtest
// out of the box. If THIS test ever fails, either NoopHost has
// regressed (unlikely — three lines per method) or the suite has
// acquired a bug that would also flag legitimate third-party
// hosts.

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/agent/agenttest"
)

func TestHostSuite_PassesNoopHost(t *testing.T) {
	agenttest.HostSuite(t, func() agent.Host { return agent.NoopHost{} })
}
