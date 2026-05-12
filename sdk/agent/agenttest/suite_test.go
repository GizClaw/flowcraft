package agenttest_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/agent/agenttest"
	"github.com/GizClaw/flowcraft/sdk/engine"
)

// TestDeciderSuite_PassesBaseDecider asserts the no-op
// [agent.BaseDecider] satisfies every contract probe — embedding
// BaseDecider must remain a safe way to scaffold custom deciders.
func TestDeciderSuite_PassesBaseDecider(t *testing.T) {
	agenttest.DeciderSuite(t, func() agent.Decider { return agent.BaseDecider{} })
}

// TestDeciderSuite_PassesDiscardOnInterruptCauses asserts the
// canonical disposition decider [agent.DiscardOnInterruptCauses]
// remains contract-compliant: stateless, mutation-free,
// concurrency-safe, panic-free across every Status.
func TestDeciderSuite_PassesDiscardOnInterruptCauses(t *testing.T) {
	agenttest.DeciderSuite(t, func() agent.Decider {
		return agent.NewDiscardOnInterruptCauses("barge-in",
			engine.CauseUserInput, engine.CauseUserCancel)
	})
}

// TestObserverSuite_PassesBaseObserver asserts the no-op
// [agent.BaseObserver] satisfies every contract probe — embedding
// BaseObserver must remain a safe scaffolding choice.
func TestObserverSuite_PassesBaseObserver(t *testing.T) {
	agenttest.ObserverSuite(t, func() agent.Observer { return agent.BaseObserver{} })
}
