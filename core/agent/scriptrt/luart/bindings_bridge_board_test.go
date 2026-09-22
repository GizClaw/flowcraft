package luart

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/bindings"
	"github.com/GizClaw/flowcraft/core/message"
)

// luaSteerHost is the minimal agent.Host carrying a steer queue, so the
// guide's canonical steer node can run under Lua as written.
type luaSteerHost struct {
	agent.NoopHost
	queue []message.Message
}

func (h *luaSteerHost) DrainSteer() []message.Message {
	out := h.queue
	h.queue = nil
	return out
}

// TestBoardBindings_SteerNodeAndBatchAppend runs the canonical steer node
// from docs/guides/graph.md under Lua, where the batch form needs care: a
// table is the only container, so an empty queue and an empty batch both
// arrive at the Go boundary as an empty map.
func TestBoardBindings_SteerNodeAndBatchAppend(t *testing.T) {
	rt := New(WithPoolSize(1))
	board := agent.NewBoard()
	host := &luaSteerHost{}
	env, err := bindings.BuildEnv(context.Background(), nil,
		bindings.NewBoardBridge(board),
		bindings.NewHostBridge(host, "steer", nil),
	)
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}

	// The documented node, verbatim: bind the drain, then append the
	// batch. Binding first is what makes it portable — in argument
	// position the call would expand to both of its return values.
	const steerNode = `
		local pending = host.drainSteer()
		board.appendChannel(board.MAIN_CHANNEL, pending)
	`
	if _, err := rt.Exec(context.Background(), "steer-idle", steerNode, env); err != nil {
		t.Fatalf("steer node on an idle round: %v", err)
	}
	if got := board.ChannelLen(agent.MainChannel); got != 0 {
		t.Fatalf("idle steer appended %d messages", got)
	}

	// A correction arrived while the round was in flight: the batch lands
	// in queue order.
	host.queue = []message.Message{
		message.NewTextMessage(message.RoleUser, "use the vendored copy"),
		message.NewTextMessage(message.RoleUser, "then re-run the suite"),
	}
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

	// A bare empty table is the empty batch too (Lua has no separate
	// array type, so this is what an empty queue collapses to). The
	// binding reports success as a nil return value.
	if _, err := rt.Exec(context.Background(), "empty-batch", `
		local res = board.appendChannel(board.MAIN_CHANNEL, {})
		if res ~= nil then error("an empty batch returned " .. tostring(res)) end
	`, env); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	if got := board.ChannelLen(agent.MainChannel); got != 2 {
		t.Fatalf("empty batch changed the channel: %d messages", got)
	}

	// setChannel reads an empty table as the empty list too, which is the
	// only way a Lua script can clear a channel.
	if _, err := rt.Exec(context.Background(), "clear", `
		local res = board.setChannel(board.MAIN_CHANNEL, {})
		if res ~= nil then error("setChannel(empty) returned " .. tostring(res)) end
		if board.channelLen(board.MAIN_CHANNEL) ~= 0 then error("setChannel(empty) did not clear the channel") end
	`, env); err != nil {
		t.Fatalf("setChannel(empty): %v", err)
	}
}

// TestBoardBindings_NarrowReads covers the narrow board reads under Lua:
// a missing channel reads as nil (Lua's null) and the tail keeps channel
// order, with Lua's 1-based indexing.
func TestBoardBindings_NarrowReads(t *testing.T) {
	rt := New(WithPoolSize(1))
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleUser, "one"))
	board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleAssistant, "two"))

	env, err := bindings.BuildEnv(context.Background(), nil, bindings.NewBoardBridge(board))
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}
	if _, err := rt.Exec(context.Background(), "narrow", `
		if board.channelLen(board.MAIN_CHANNEL) ~= 2 then error("channelLen") end
		if board.channelLen("missing") ~= 0 then error("channelLen on a missing channel") end

		local last = board.lastMessage(board.MAIN_CHANNEL)
		if last == nil then error("lastMessage returned nothing") end
		if last.role ~= "assistant" then error("lastMessage role: " .. tostring(last.role)) end
		if last.content.parts[1].text ~= "two" then error("lastMessage text") end
		if board.lastMessage("missing") ~= nil then error("lastMessage on a missing channel must be nil") end

		local tail = board.channelTail(board.MAIN_CHANNEL, 1)
		if #tail ~= 1 then error("channelTail length: " .. #tail) end
		if tail[1].content.parts[1].text ~= "two" then error("channelTail text") end
		if #board.channelTail("missing", 2) ~= 0 then error("channelTail on a missing channel") end
	`, env); err != nil {
		t.Fatalf("narrow reads: %v", err)
	}
}

// TestBoardBindings_BatchValidationIsAllOrNothing pins the Lua half of the
// batch contract: a batch that fails validation reports the error as the
// binding's return value — Lua has no exception to catch here — and lands
// none of it.
func TestBoardBindings_BatchValidationIsAllOrNothing(t *testing.T) {
	rt := New(WithPoolSize(1))
	board := agent.NewBoard()
	env, err := bindings.BuildEnv(context.Background(), nil, bindings.NewBoardBridge(board))
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}
	if _, err := rt.Exec(context.Background(), "bad-batch", `
		local good = { role = "user", content = { parts = { { type = "text", text = "one" } } } }
		local bad = { role = "bogus", content = { parts = { { type = "text", text = "two" } } } }
		local before = board.channelLen(board.MAIN_CHANNEL)
		local res = board.appendChannel(board.MAIN_CHANNEL, { good, bad })
		if res == nil then error("a bad batch must report an error through the return value") end
		if board.channelLen(board.MAIN_CHANNEL) ~= before then error("a failed batch landed messages") end
	`, env); err != nil {
		t.Fatalf("bad batch: %v", err)
	}
	if got := board.ChannelLen(agent.MainChannel); got != 0 {
		t.Fatalf("a failed batch left %d messages", got)
	}
}
