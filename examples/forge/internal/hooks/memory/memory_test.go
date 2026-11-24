package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// seatRoot is the workspace a seat's memory lives in. A hook takes no substrate: the directory is the
// document's, absolute, and the files are plain ones under it.
func seatRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// hooksFor builds the hook the way a document does: settings in, hooks out.
func hooksFor(t *testing.T, root, agentName, extra string) agent.Hooks {
	t.Helper()
	settings := `{"dir":"` + root + `"`
	if extra != "{}" {
		settings += "," + strings.TrimSuffix(strings.TrimPrefix(extra, "{"), "}")
	}
	settings += "}"
	factory := Impl()
	planned, err := factory.Plan(json.RawMessage(settings))
	if err != nil {
		t.Fatalf("plan %s: %v", settings, err)
	}
	built, err := factory.Make(planned)
	if err != nil {
		t.Fatalf("make: %v", err)
	}
	hooks, ok := built.(agent.Hooks)
	if !ok {
		t.Fatalf("built %T, want agent.Hooks", built)
	}
	return hooks
}

// brief is a delegation's brief: the game id on the first line, then what was asked.
func brief(game, text string) message.Message {
	return message.NewTextMessage(message.RoleUser, "game="+game+"\n"+text)
}

// seatSession is the session id a delegated seat actually sees: the table's session, the delegation
// marker, and the call that asked.
func seatSession(root string, call int) string {
	return root + "/d/seat_5/script:" + string(rune('0'+call)) + ":delegate"
}

const (
	tableOne = "referee/main"
	tableTwo = "referee/main-2"
)

func TestMemoryReplaysWhatTheSeatSaid(t *testing.T) {
	root := seatRoot(t)
	hooks := hooksFor(t, root, "seat_5", "{}")
	ctx := context.Background()

	// First turn: nothing remembered, so the brief is untouched.
	turn := &agent.TurnInfo{Agent: "seat_5", SessionID: seatSession(tableOne, 0), Message: brief("abc", "请发言")}
	if err := hooks.BeforeTurn(ctx, turn); err != nil {
		t.Fatalf("BeforeTurn: %v", err)
	}
	if strings.Contains(turn.Message.Content.Text(), recapHeader) {
		t.Fatal("the first turn has no memory to replay")
	}
	if err := hooks.AfterTurn(ctx, *turn, agent.TurnOutcome{
		Content: message.NewTextContent("我怀疑5号，他昨天太急着归票。"),
	}); err != nil {
		t.Fatalf("AfterTurn: %v", err)
	}

	file := filepath.Join(root, "referee_main", "seat_5.json")
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("memory file: %v", err)
	}

	// Second turn, same game: the seat's own line comes back as context.
	turn = &agent.TurnInfo{Agent: "seat_5", SessionID: seatSession(tableOne, 1), Message: brief("abc", "请投票")}
	if err := hooks.BeforeTurn(ctx, turn); err != nil {
		t.Fatalf("BeforeTurn: %v", err)
	}
	if got := turn.Message.Content.Text(); !strings.Contains(got, "我怀疑5号") {
		t.Fatalf("brief = %q, want the recalled line", got)
	}
}

func TestMemoryIsPerGame(t *testing.T) {
	root := seatRoot(t)
	hooks := hooksFor(t, root, "seat_5", "{}")
	ctx := context.Background()

	turn := &agent.TurnInfo{Agent: "seat_5", SessionID: seatSession(tableOne, 0), Message: brief("abc", "请发言")}
	if err := hooks.AfterTurn(ctx, *turn, agent.TurnOutcome{Content: message.NewTextContent("上一局的话")}); err != nil {
		t.Fatalf("AfterTurn: %v", err)
	}
	next := &agent.TurnInfo{Agent: "seat_5", SessionID: seatSession(tableTwo, 0), Message: brief("abc", "请发言")}
	if err := hooks.BeforeTurn(ctx, next); err != nil {
		t.Fatalf("BeforeTurn: %v", err)
	}
	if strings.Contains(next.Message.Content.Text(), "上一局的话") {
		t.Fatal("a new game must start from a clean page")
	}
}

func TestMemoryIsPerSeat(t *testing.T) {
	root := seatRoot(t)
	five := hooksFor(t, root, "seat_5", "{}")
	seven := hooksFor(t, root, "seat_7", "{}")
	ctx := context.Background()

	turn := &agent.TurnInfo{Agent: "seat_5", SessionID: seatSession(tableOne, 0), Message: brief("abc", "请发言")}
	if err := five.AfterTurn(ctx, *turn, agent.TurnOutcome{Content: message.NewTextContent("五号的话")}); err != nil {
		t.Fatalf("AfterTurn: %v", err)
	}
	other := &agent.TurnInfo{Agent: "seat_7", SessionID: seatSession(tableOne, 0), Message: brief("abc", "请发言")}
	if err := seven.BeforeTurn(ctx, other); err != nil {
		t.Fatalf("BeforeTurn: %v", err)
	}
	if strings.Contains(other.Message.Content.Text(), "五号的话") {
		t.Fatal("one seat must not read another's page")
	}
}

func TestMemoryKeepsTheLastLines(t *testing.T) {
	root := seatRoot(t)
	hooks := hooksFor(t, root, "seat_5", `{"keep": 2}`)
	ctx := context.Background()
	for _, said := range []string{"第一句", "第二句", "第三句"} {
		turn := &agent.TurnInfo{Agent: "seat_5", SessionID: seatSession(tableOne, 0), Message: brief("abc", "请发言")}
		if err := hooks.AfterTurn(ctx, *turn, agent.TurnOutcome{Content: message.NewTextContent(said)}); err != nil {
			t.Fatalf("AfterTurn: %v", err)
		}
	}
	turn := &agent.TurnInfo{Agent: "seat_5", SessionID: seatSession(tableOne, 0), Message: brief("abc", "请发言")}
	if err := hooks.BeforeTurn(ctx, turn); err != nil {
		t.Fatalf("BeforeTurn: %v", err)
	}
	text := turn.Message.Content.Text()
	if strings.Contains(text, "第一句") {
		t.Fatalf("brief = %q, the oldest line should have been dropped", text)
	}
	if !strings.Contains(text, "第二句") || !strings.Contains(text, "第三句") {
		t.Fatalf("brief = %q, want the last two lines", text)
	}
}

func TestMemoryIgnoresABriefWithoutAGame(t *testing.T) {
	root := seatRoot(t)
	hooks := hooksFor(t, root, "seat_5", "{}")
	turn := &agent.TurnInfo{Agent: "seat_5", SessionID: seatSession(tableOne, 0),
		Message: message.NewTextMessage(message.RoleUser, "没有游戏号")}
	if err := hooks.BeforeTurn(context.Background(), turn); err != nil {
		t.Fatalf("BeforeTurn: %v", err)
	}
	if err := hooks.AfterTurn(context.Background(), *turn, agent.TurnOutcome{Content: message.NewTextContent("x")}); err != nil {
		t.Fatalf("AfterTurn: %v", err)
	}
	if turn.Message.Content.Text() != "没有游戏号" {
		t.Fatalf("message = %q, want it untouched", turn.Message.Content.Text())
	}
}

// The directory is the document's and must be absolute: a backend owns no directory, so a relative one
// would mean "somewhere under whatever the process happens to be", which is not a place a memory can
// live.
func TestBuildRefusesUnusableSettings(t *testing.T) {
	for name, settings := range map[string]string{
		"relative dir": `{"dir": ".forge/memory"}`,
		"empty dir":    `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			factory := Impl()
			planned, err := factory.Plan(json.RawMessage(settings))
			if err == nil {
				_, err = factory.Make(planned)
			}
			if err == nil {
				t.Fatal("expected the settings to be refused")
			}
		})
	}
}
