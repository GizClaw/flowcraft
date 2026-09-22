package jsrt_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/agent/bindings"
	"github.com/GizClaw/flowcraft/core/agent/scriptrt/jsrt"
	"github.com/GizClaw/flowcraft/core/message"
)

// TestBoardBridge_CanonicalSteerNode runs the steer node documented in
// docs/guides/graph.md verbatim over the real board and host bridges:
// bind the drain, then append the batch. The guide ships the same shape
// for the Lua runtime, so this is the JS half of that claim.
func TestBoardBridge_CanonicalSteerNode(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := agent.NewBoard()
	host := agenttest.NewMockHost()

	env := buildEnv(t, nil, bindings.NewBoardBridge(board), bindings.NewHostBridge(host, "steer", nil))
	const steerNode = `
		var pending = host.drainSteer();
		board.appendChannel(board.MAIN_CHANNEL, pending);
	`

	// An idle round: the drained array is empty, so the node appends
	// nothing and is not an error.
	if _, err := rt.Exec(context.Background(), "steer-idle", steerNode, env); err != nil {
		t.Fatalf("steer node on an idle round: %v", err)
	}
	if got := board.ChannelLen(agent.MainChannel); got != 0 {
		t.Fatalf("idle steer appended %d messages", got)
	}

	// A correction arrived while the round was in flight: the batch lands
	// in queue order.
	host.Steer(message.NewTextMessage(message.RoleUser, "use the vendored copy"))
	host.Steer(message.NewTextMessage(message.RoleUser, "then re-run the suite"))
	if _, err := rt.Exec(context.Background(), "steer", steerNode, env); err != nil {
		t.Fatalf("steer node: %v", err)
	}
	msgs := board.Channel(agent.MainChannel)
	if len(msgs) != 2 {
		t.Fatalf("steer node appended %d messages, want 2", len(msgs))
	}
	for i, want := range []string{"use the vendored copy", "then re-run the suite"} {
		if got := msgs[i].Content.Text(); got != want {
			t.Errorf("steered message %d = %q, want %q", i, got, want)
		}
	}
}
