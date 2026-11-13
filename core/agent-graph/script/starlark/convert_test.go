package starlark

import (
	"testing"

	star "go.starlark.net/starlark"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

func TestContentTextTakesTheSinglePart(t *testing.T) {
	content := message.NewTextContent("你好")
	if got := contentText(content); got != "你好" {
		t.Fatalf("text = %q", got)
	}
}

func TestContentTextFallsBackToTheContract(t *testing.T) {
	// Several parts are concatenated by Content.Text, which is the fallback: the
	// node does not re-implement the contract for the shapes it does not special
	// case.
	content := message.Content{Parts: []message.Part{
		message.TextPart{Text: "你"},
		message.TextPart{Text: "好"},
	}}
	if got := contentText(content); got != "你好" {
		t.Fatalf("text = %q", got)
	}
	if got := contentText(message.Content{}); got != "" {
		t.Fatalf("empty content = %q", got)
	}
}

func TestMessagesBecomeDictsWithTheDeclaredShape(t *testing.T) {
	history := graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
		message.NewTextMessage(message.RoleUser, "第一句"),
		message.NewTextMessage(message.RoleAssistant, "第二句"),
	}))
	collapsed, err := history.Messages()
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	value, err := toStarlark(script.Messages{History: collapsed}, nil)
	if err != nil {
		t.Fatalf("toStarlark: %v", err)
	}
	if value.Type() != "messages" {
		t.Fatalf("type = %q", value.Type())
	}
	if got := value.String(); got != `[{"role": "user", "text": "第一句"}, {"role": "assistant", "text": "第二句"}]` {
		t.Fatalf("value = %s", got)
	}
}

// TestJSONCrossesAsBytes pins the read path the projection optimisation changed: a
// JSON state value arrives as the bytes it already is, and becomes a Starlark value
// here rather than after a reflective round trip.
func TestJSONCrossesAsBytes(t *testing.T) {
	value, err := toStarlark(json.RawMessage(`{"phase":"day","n":2,"open":true}`), nil)
	if err != nil {
		t.Fatalf("toStarlark: %v", err)
	}
	dict, ok := value.(*star.Dict)
	if !ok {
		t.Fatalf("value = %T, want a dict", value)
	}
	phase, _, err := dict.Get(star.String("phase"))
	if err != nil {
		t.Fatalf("phase: %v", err)
	}
	if text, _ := star.AsString(phase); text != "day" {
		t.Fatalf("phase = %v", phase)
	}
	number, _, err := dict.Get(star.String("n"))
	if err != nil {
		t.Fatalf("n: %v", err)
	}
	if number.String() != "2" {
		t.Fatalf("n = %v", number)
	}
	if _, err := toStarlark(json.RawMessage(`{`), nil); err == nil {
		t.Fatal("a broken document must fail the projection")
	}
}
