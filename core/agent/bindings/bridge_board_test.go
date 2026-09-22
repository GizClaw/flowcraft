package bindings

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// boardSurface builds the script-side board surface over board. Tests
// call the exposed functions directly: the VM-level contract (JS
// arrays, numbers, null) is covered by the runtime tests next to the
// engines.
func boardSurface(t *testing.T, board *agent.Board) map[string]any {
	t.Helper()
	env := mustBuildEnv(t, nil, NewBoardBridge(board))
	surface, ok := env.Bindings["board"].(map[string]any)
	if !ok {
		t.Fatalf("board binding = %T", env.Bindings["board"])
	}
	return surface
}

// textWire builds the script-side wire shape of a one-part text message.
func textWire(role, text string) map[string]any {
	return map[string]any{
		"role":    role,
		"content": map[string]any{"parts": []any{map[string]any{"type": "text", "text": text}}},
	}
}

// wireText reads the text of the first part out of a projected message.
func wireText(t *testing.T, msg any) string {
	t.Helper()
	m, ok := msg.(map[string]any)
	if !ok {
		t.Fatalf("message = %T, want map[string]any", msg)
	}
	content, ok := m["content"].(map[string]any)
	if !ok {
		t.Fatalf("content = %T, want map[string]any", m["content"])
	}
	parts, ok := content["parts"].([]any)
	if !ok || len(parts) == 0 {
		t.Fatalf("parts = %v, want a non-empty array", content["parts"])
	}
	part, ok := parts[0].(map[string]any)
	if !ok {
		t.Fatalf("part = %T, want map[string]any", parts[0])
	}
	text, _ := part["text"].(string)
	return text
}

func TestBoardBridge_NarrowChannelReads(t *testing.T) {
	board := agent.NewBoard()
	surface := boardSurface(t, board)
	channelLen := surface["channelLen"].(func(string) int)
	lastMessage := surface["lastMessage"].(func(string) (any, error))
	channelTail := surface["channelTail"].(func(string, int) ([]any, error))
	appendChannel := surface["appendChannel"].(func(string, any) error)

	// Empty channels: a count of zero, no last message, an empty tail.
	if got := channelLen("main"); got != 0 {
		t.Errorf("channelLen on empty channel = %d, want 0", got)
	}
	if last, err := lastMessage("main"); err != nil || last != nil {
		t.Errorf("lastMessage on empty channel = (%v, %v), want (nil, nil)", last, err)
	}
	if tail, err := channelTail("main", 3); err != nil || len(tail) != 0 {
		t.Errorf("channelTail on empty channel = (%v, %v), want empty", tail, err)
	}

	for _, text := range []string{"one", "two"} {
		if err := appendChannel("main", textWire("user", text)); err != nil {
			t.Fatalf("appendChannel(%q): %v", text, err)
		}
	}
	if got := channelLen("main"); got != 2 {
		t.Errorf("channelLen = %d, want 2", got)
	}

	last, err := lastMessage("main")
	if err != nil {
		t.Fatalf("lastMessage: %v", err)
	}
	if got := wireText(t, last); got != "two" {
		t.Errorf("lastMessage text = %q, want %q", got, "two")
	}

	tail, err := channelTail("main", 1)
	if err != nil {
		t.Fatalf("channelTail: %v", err)
	}
	if len(tail) != 1 || wireText(t, tail[0]) != "two" {
		t.Errorf("channelTail(1) = %v, want the last message", tail)
	}
	// A count past the channel's length yields the whole channel, in order.
	tail, err = channelTail("main", 99)
	if err != nil {
		t.Fatalf("channelTail(99): %v", err)
	}
	if len(tail) != 2 || wireText(t, tail[0]) != "one" || wireText(t, tail[1]) != "two" {
		t.Errorf("channelTail(99) = %v, want [one two]", tail)
	}
	if tail, err := channelTail("main", 0); err != nil || len(tail) != 0 {
		t.Errorf("channelTail(0) = (%v, %v), want empty", tail, err)
	}
}

func TestBoardBridge_AppendChannelBatch(t *testing.T) {
	board := agent.NewBoard()
	surface := boardSurface(t, board)
	appendChannel := surface["appendChannel"].(func(string, any) error)
	batch := []any{textWire("user", "one"), textWire("assistant", "two")}
	if err := appendChannel("main", batch); err != nil {
		t.Fatalf("appendChannel(batch): %v", err)
	}
	msgs := board.Channel("main")
	if len(msgs) != 2 || msgs[0].Content.Text() != "one" || msgs[1].Content.Text() != "two" {
		t.Fatalf("batch landed as %+v, want [one two] in order", msgs)
	}

	// The batch is copied on the way in: the script may reuse its array.
	batch[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"] = "MUTATED"
	if got := board.Channel("main")[0].Content.Text(); got != "one" {
		t.Errorf("batch append aliased the script array: %q", got)
	}

	// A batch validates as a whole: a bad message lands none of it.
	err := appendChannel("main", []any{textWire("user", "three"), textWire("bogus", "four")})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("bad batch err = %v, want a validation error", err)
	}
	if got := board.ChannelLen("main"); got != 2 {
		t.Errorf("failed batch appended: len = %d, want 2", got)
	}

	// Empty batches are no-ops; a single object still appends one message.
	if err := appendChannel("main", []any{}); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	if err := appendChannel("main", textWire("user", "five")); err != nil {
		t.Fatalf("single message: %v", err)
	}
	if got := board.ChannelLen("main"); got != 3 {
		t.Errorf("len after no-op + single = %d, want 3", got)
	}
}

func TestBoardBridgeResolve(t *testing.T) {
	board := agent.NewBoard()
	board.SetVar("user", map[string]any{"name": "ada"})
	board.SetVar("n", float64(3))

	env := mustBuildEnv(t, nil, NewBoardBridge(board))
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
