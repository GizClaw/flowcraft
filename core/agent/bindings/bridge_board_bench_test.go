package bindings

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// Conversations in the field are dominated by payload bytes: a long
// session carries hundreds of messages whose tool results and summaries
// run to kilobytes each. These numbers shape a channel of that kind —
// ~512 KB of text spread over 64 messages — so read costs are measured
// against something with a realistic size.
const (
	benchChannelMessages = 64
	benchMessageBytes    = 8 << 10
)

// newBenchBoard builds a board carrying benchChannelMessages messages
// plus the script-side surface over it.
func newBenchBoard(tb testing.TB, messages int) (*agent.Board, map[string]any) {
	tb.Helper()
	board := agent.NewBoard()
	body := strings.Repeat("x", benchMessageBytes)
	for i := 0; i < messages; i++ {
		role := message.RoleUser
		if i%2 == 1 {
			role = message.RoleAssistant
		}
		board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(role, body))
	}
	env, err := BuildEnv(context.Background(), nil, NewBoardBridge(board))
	if err != nil {
		tb.Fatalf("BuildEnv: %v", err)
	}
	surface, ok := env.Bindings["board"].(map[string]any)
	if !ok {
		tb.Fatalf("board binding = %T", env.Bindings["board"])
	}
	return board, surface
}

func BenchmarkBoardChannelProjection(b *testing.B) {
	_, surface := newBenchBoard(b, benchChannelMessages)
	channel := surface["channel"].(func(string) ([]any, error))

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := channel(agent.MainChannel); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBoardChannelLen(b *testing.B) {
	_, surface := newBenchBoard(b, benchChannelMessages)
	channelLen := surface["channelLen"].(func(string) int)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = channelLen(agent.MainChannel)
	}
}

func BenchmarkBoardLastMessage(b *testing.B) {
	_, surface := newBenchBoard(b, benchChannelMessages)
	lastMessage := surface["lastMessage"].(func(string) (any, error))

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := lastMessage(agent.MainChannel); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBoardChannelTail(b *testing.B) {
	_, surface := newBenchBoard(b, benchChannelMessages)
	channelTail := surface["channelTail"].(func(string, int) ([]any, error))

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := channelTail(agent.MainChannel, 1); err != nil {
			b.Fatal(err)
		}
	}
}

// TestBoardBridge_NarrowReadsDoNotScaleWithChannel pins what the narrow
// accessors are for: reading a length or a tail must not pay for the
// whole conversation, so a node that peeks at the end of a long channel
// stays cheap as the channel grows. Allocation counts are the proxy —
// they are what the profiling that motivated the accessors measured.
func TestBoardBridge_NarrowReadsDoNotScaleWithChannel(t *testing.T) {
	_, surface := newBenchBoard(t, benchChannelMessages)
	channel := surface["channel"].(func(string) ([]any, error))
	channelLen := surface["channelLen"].(func(string) int)
	lastMessage := surface["lastMessage"].(func(string) (any, error))
	channelTail := surface["channelTail"].(func(string, int) ([]any, error))

	full := testing.AllocsPerRun(20, func() {
		if _, err := channel(agent.MainChannel); err != nil {
			t.Errorf("channel: %v", err)
		}
	})
	narrow := map[string]float64{
		"channelLen": testing.AllocsPerRun(20, func() {
			if channelLen(agent.MainChannel) != benchChannelMessages {
				t.Errorf("channelLen returned the wrong length")
			}
		}),
		"lastMessage": testing.AllocsPerRun(20, func() {
			if _, err := lastMessage(agent.MainChannel); err != nil {
				t.Errorf("lastMessage: %v", err)
			}
		}),
		"channelTail(1)": testing.AllocsPerRun(20, func() {
			if _, err := channelTail(agent.MainChannel, 1); err != nil {
				t.Errorf("channelTail: %v", err)
			}
		}),
	}

	if narrow["channelLen"] != 0 {
		t.Errorf("channelLen allocates %.1f per read, want 0 (the count is all it needs)", narrow["channelLen"])
	}
	for name, got := range narrow {
		if got*10 > full {
			t.Errorf("%s allocates %.0f per read while the full projection allocates %.0f: "+
				"a narrow read must stay an order of magnitude below a whole-channel projection",
				name, got, full)
		}
	}
}
