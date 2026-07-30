package agenttest_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/agent/agenttest"
)

// TestDeciderSuite_PassesBaseDecider asserts the no-op
// [agent.BaseAfterExecute] satisfies every contract probe — embedding
// BaseAfterExecute must remain a safe way to scaffold custom deciders.
func TestDeciderSuite_PassesBaseDecider(t *testing.T) {
	agenttest.AfterExecuteSuite(t, func() agent.AfterExecute { return agent.BaseAfterExecute{} })
}

// TestDeciderSuite_PassesDiscardOnInterruptCauses asserts the
// canonical disposition decider [agent.DiscardOnInterruptCauses]
// remains contract-compliant: stateless, mutation-free,
// concurrency-safe, panic-free across every Status.
func TestDeciderSuite_PassesDiscardOnInterruptCauses(t *testing.T) {
	agenttest.AfterExecuteSuite(t, func() agent.AfterExecute {
		return agent.NewDiscardOnInterruptCauses("barge-in",
			agent.CauseUserInput, agent.CauseUserCancel)
	})
}

// TestObserverSuite_PassesBaseObserver asserts the no-op
// [agent.BaseHook] satisfies every contract probe — embedding
// BaseHook must remain a safe scaffolding choice.
func TestObserverSuite_PassesBaseObserver(t *testing.T) {
	agenttest.HookSuite(t, func() agent.Hook { return agent.BaseHook{} })
}
