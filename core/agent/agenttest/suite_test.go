package agenttest_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
)

// TestDeciderSuite_PassesBaseDecider asserts the no-op
// [agent.BaseReferee] satisfies every contract probe — embedding
// BaseReferee must remain a safe way to scaffold custom deciders.
func TestDeciderSuite_PassesBaseDecider(t *testing.T) {
	agenttest.RefereeSuite(t, func() agent.Referee { return agent.BaseReferee{} })
}

// TestDeciderSuite_PassesDiscardOnInterruptCauses asserts the
// canonical disposition decider [agent.DiscardOnInterruptCauses]
// remains contract-compliant: stateless, mutation-free,
// concurrency-safe, panic-free across every Status.
func TestDeciderSuite_PassesDiscardOnInterruptCauses(t *testing.T) {
	agenttest.RefereeSuite(t, func() agent.Referee {
		return agent.NewDiscardOnInterruptCauses("barge-in",
			agent.CauseUserInput, agent.CauseUserCancel)
	})
}

// TestObserverSuite_PassesBaseObserver asserts the no-op
// [agent.BaseObserver] satisfies every contract probe — embedding
// BaseObserver must remain a safe scaffolding choice.
func TestObserverSuite_PassesBaseObserver(t *testing.T) {
	agenttest.ObserverSuite(t, func() agent.Observer { return agent.BaseObserver{} })
}
