package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// seatRuntime is the substrate a seat has: one writable root, which is where its
// memory file lives.
func seatRuntime(t *testing.T) (*runtime.Runtime, string) {
	t.Helper()
	root := t.TempDir()
	rt, err := runtime.New(context.Background(), runtime.Config{
		Version: 1,
		Sandbox: runtime.SandboxConfig{Backend: runtime.BackendLocal, Writes: []string{root}},
		Files:   runtime.FilesConfig{Roots: []string{root}},
		Tools:   runtime.ToolsConfig{Enable: []string{"files"}},
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt, root
}

func hooksFor(t *testing.T, rt *runtime.Runtime, agentName, settings string) agent.Hooks {
	t.Helper()
	built, err := build(context.Background(), deploy.HookDeps{
		Agent:   agentName,
		Spec:    deploy.HookSpec{Kind: KindName, Settings: json.RawMessage(settings)},
		Runtime: rt,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return built
}

func brief(game, text string) message.Message {
	return message.NewTextMessage(message.RoleUser, "game="+game+"\n"+text)
}

func TestMemoryReplaysWhatTheSeatSaid(t *testing.T) {
	rt, root := seatRuntime(t)
	hooks := hooksFor(t, rt, "seat_5", `{}`)
	ctx := context.Background()

	// First turn: nothing remembered, so the brief is untouched.
	turn := agent.Turn{Agent: "seat_5", SessionID: "child-1", Message: brief("abc", "请发言")}
	if err := hooks.BeforeTurn(ctx, &turn); err != nil {
		t.Fatalf("BeforeTurn: %v", err)
	}
	if strings.Contains(turn.Message.Content.Text(), recapHeader) {
		t.Fatal("the first turn has no memory to replay")
	}
	if err := hooks.AfterTurn(ctx, turn, agent.Result{
		Content: message.NewTextContent("我怀疑5号，他昨天太急着归票。"),
	}); err != nil {
		t.Fatalf("AfterTurn: %v", err)
	}

	path := filepath.Join(root, ".forge", "memory", "abc", "seat_5.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("memory file: %v", err)
	}

	// Second turn, same game: the seat's own line comes back as context.
	turn = agent.Turn{Agent: "seat_5", SessionID: "child-2", Message: brief("abc", "请投票")}
	if err := hooks.BeforeTurn(ctx, &turn); err != nil {
		t.Fatalf("BeforeTurn: %v", err)
	}
	if got := turn.Message.Content.Text(); !strings.Contains(got, "我怀疑5号") {
		t.Fatalf("brief = %q, want the recalled line", got)
	}
}

func TestMemoryIsPerGame(t *testing.T) {
	rt, _ := seatRuntime(t)
	hooks := hooksFor(t, rt, "seat_5", `{}`)
	ctx := context.Background()

	turn := agent.Turn{Agent: "seat_5", SessionID: "child-1", Message: brief("abc", "请发言")}
	if err := hooks.AfterTurn(ctx, turn, agent.Result{Content: message.NewTextContent("上一局的话")}); err != nil {
		t.Fatalf("AfterTurn: %v", err)
	}
	next := agent.Turn{Agent: "seat_5", SessionID: "child-2", Message: brief("xyz", "请发言")}
	if err := hooks.BeforeTurn(ctx, &next); err != nil {
		t.Fatalf("BeforeTurn: %v", err)
	}
	if strings.Contains(next.Message.Content.Text(), "上一局的话") {
		t.Fatal("a new game must start from a clean page")
	}
}

func TestMemoryIsPerSeat(t *testing.T) {
	rt, _ := seatRuntime(t)
	five := hooksFor(t, rt, "seat_5", `{}`)
	seven := hooksFor(t, rt, "seat_7", `{}`)
	ctx := context.Background()

	turn := agent.Turn{Agent: "seat_5", SessionID: "c", Message: brief("abc", "请发言")}
	if err := five.AfterTurn(ctx, turn, agent.Result{Content: message.NewTextContent("五号的话")}); err != nil {
		t.Fatalf("AfterTurn: %v", err)
	}
	other := agent.Turn{Agent: "seat_7", SessionID: "c", Message: brief("abc", "请发言")}
	if err := seven.BeforeTurn(ctx, &other); err != nil {
		t.Fatalf("BeforeTurn: %v", err)
	}
	if strings.Contains(other.Message.Content.Text(), "五号的话") {
		t.Fatal("one seat must not read another's page")
	}
}

func TestMemoryKeepsTheLastLines(t *testing.T) {
	rt, _ := seatRuntime(t)
	hooks := hooksFor(t, rt, "seat_5", `{"keep": 2}`)
	ctx := context.Background()
	for _, said := range []string{"第一句", "第二句", "第三句"} {
		turn := agent.Turn{Agent: "seat_5", SessionID: "c", Message: brief("abc", "请发言")}
		if err := hooks.AfterTurn(ctx, turn, agent.Result{Content: message.NewTextContent(said)}); err != nil {
			t.Fatalf("AfterTurn: %v", err)
		}
	}
	turn := agent.Turn{Agent: "seat_5", SessionID: "c", Message: brief("abc", "请发言")}
	if err := hooks.BeforeTurn(ctx, &turn); err != nil {
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
	rt, _ := seatRuntime(t)
	hooks := hooksFor(t, rt, "seat_5", `{}`)
	turn := agent.Turn{Agent: "seat_5", SessionID: "c", Message: message.NewTextMessage(message.RoleUser, "没有游戏号")}
	if err := hooks.BeforeTurn(context.Background(), &turn); err != nil {
		t.Fatalf("BeforeTurn: %v", err)
	}
	if err := hooks.AfterTurn(context.Background(), turn, agent.Result{Content: message.NewTextContent("x")}); err != nil {
		t.Fatalf("AfterTurn: %v", err)
	}
	if turn.Message.Content.Text() != "没有游戏号" {
		t.Fatalf("message = %q, want it untouched", turn.Message.Content.Text())
	}
}

func TestBuildRefusesUnusableSettings(t *testing.T) {
	rt, _ := seatRuntime(t)
	for name, settings := range map[string]string{
		"absolute dir": `{"dir": "/tmp/x"}`,
		"escaping dir": `{"dir": "../outside"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := build(context.Background(), deploy.HookDeps{
				Agent:   "seat_5",
				Spec:    deploy.HookSpec{Kind: KindName, Settings: json.RawMessage(settings)},
				Runtime: rt,
			}); err == nil {
				t.Fatal("expected the settings to be refused")
			}
		})
	}
	if _, err := build(context.Background(), deploy.HookDeps{
		Agent: "seat_5",
		Spec:  deploy.HookSpec{Kind: KindName, Settings: json.RawMessage(`{}`)},
	}); err == nil {
		t.Fatal("a hook with no runtime has nowhere to keep a memory")
	}
}
