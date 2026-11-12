package starlark

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/message"
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
	value, err := toStarlark(script.Messages{History: collapsed})
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
