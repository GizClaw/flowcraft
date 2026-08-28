package bindings

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

func TestBoardBridgeResolve(t *testing.T) {
	board := agent.NewBoard()
	board.SetVar("user", map[string]any{"name": "ada"})
	board.SetVar("n", float64(3))

	env := BuildEnv(context.Background(), nil, NewBoardBridge(board))
	b, ok := env.Bindings["board"].(map[string]any)
	if !ok {
		t.Fatalf("board binding = %T", env.Bindings["board"])
	}
	resolve := b["resolve"].(func(string) (any, error))
	resolveString := b["resolveString"].(func(string) (string, error))

	v, err := resolve("${board:user.name}")
	if err != nil || v != "ada" {
		t.Fatalf("resolve = %v, %v", v, err)
	}
	v, err = resolve("${board:n}")
	if err != nil || v != float64(3) {
		t.Fatalf("resolve typed = %v, %v", v, err)
	}
	s, err := resolveString("n=${board:n}")
	if err != nil || s != "n=3" {
		t.Fatalf("resolveString = %q, %v", s, err)
	}
	s, err = resolveString("${board:user.name}")
	if err != nil || s != "ada" {
		t.Fatalf("resolveString typed = %q, %v", s, err)
	}
	if _, err := resolve("${board:missing}"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("missing ref err = %v, want validation error", err)
	}
	if _, err := resolveString("x ${board:missing}"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("missing embedded ref err = %v, want validation error", err)
	}
}
