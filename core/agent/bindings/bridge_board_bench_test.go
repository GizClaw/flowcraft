package bindings

import (
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
	return board, boardSurface(tb, board)
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
