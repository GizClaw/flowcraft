package a2a

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	a2aprotocol "github.com/a2aproject/a2a-go/v2/a2a"
)

// testCard is a minimal JSON-RPC AgentCard pointing at a port that is
// never dialled: every contract subtest drives an empty board, which
// completes as a local no-op before any network call.
func testCard() *a2aprotocol.AgentCard {
	return &a2aprotocol.AgentCard{
		Name: "contract-agent",
		SupportedInterfaces: []*a2aprotocol.AgentInterface{
			{
				URL:             "http://127.0.0.1:9/jsonrpc",
				ProtocolBinding: a2aprotocol.TransportProtocolJSONRPC,
				ProtocolVersion: a2aprotocol.Version,
			},
		},
	}
}

// TestEngine_Contract runs the shared agent.Engine conformance suite
// against the A2A proxy engine.
func TestEngine_Contract(t *testing.T) {
	// Construction is deterministic (fixed card + options): validate it
	// once up front so the per-subtest factory never needs *testing.T.
	// The suite's factory must return a fresh engine per subtest.
	if _, err := New(context.Background(), testCard(), WithStreamMode(StreamModeOff)); err != nil {
		t.Fatalf("New: %v", err)
	}
	agenttest.EngineSuite(t, func() (agent.Engine, agenttest.Capabilities) {
		eng, _ := New(context.Background(), testCard(), WithStreamMode(StreamModeOff))
		return eng, agenttest.Capabilities{SupportsResume: true}
	})
}

// TestEngine_ResumerContract runs the shared agent.Resumer conformance
// suite against Engine.CanResume.
func TestEngine_ResumerContract(t *testing.T) {
	if _, err := New(context.Background(), testCard(), WithStreamMode(StreamModeOff)); err != nil {
		t.Fatalf("New: %v", err)
	}
	agenttest.ResumerSuite(t, func() agent.Resumer {
		eng, _ := New(context.Background(), testCard(), WithStreamMode(StreamModeOff))
		return eng
	})
}
