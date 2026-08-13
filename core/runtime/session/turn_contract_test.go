package session

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
)

// TestTurn_ObserverContract runs the shared agent.Observer conformance
// suite against the session Turn's observer surface.
func TestTurn_ObserverContract(t *testing.T) {
	agenttest.ObserverSuite(t, func() agent.Observer {
		return newTurn(nil, "run-observer", context.Background())
	})
}

// TestTurn_RefereeContract runs the shared agent.Referee conformance
// suite against Turn.After.
func TestTurn_RefereeContract(t *testing.T) {
	agenttest.RefereeSuite(t, func() agent.Referee {
		return newTurn(nil, "run-referee", context.Background())
	})
}

// TestTurn_CommitViewProviderContract runs the shared
// agent.CommitViewProvider conformance suite against Turn.CommitView.
func TestTurn_CommitViewProviderContract(t *testing.T) {
	agenttest.CommitViewProviderSuite(t, func() agent.CommitViewProvider {
		return newTurn(nil, "run-view", context.Background())
	})
}

// TestTurn_StreamSinkContract runs the shared agent.StreamSink
// conformance suite against both production sinks: the queued sink and
// the turn's stream coordinator.
func TestTurn_StreamSinkContract(t *testing.T) {
	t.Run("QueuedSink", func(t *testing.T) {
		agenttest.StreamSinkSuite(t, func() agent.StreamSink {
			return newQueuedSink(nil, "run-sink", SinkSpec{}, 16)
		})
	})
	t.Run("Coordinator", func(t *testing.T) {
		agenttest.StreamSinkSuite(t, func() agent.StreamSink {
			turn := newTurn(nil, "run-coord", context.Background())
			return newStreamCoordinator(turn, nil, nil, 0, 0)
		})
	})
}
