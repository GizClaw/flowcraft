package nodes_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/nodes"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// countingProvider records which entry point a call used: the streaming one a
// spoken turn needs, or the finished one a call whose answer is data needs.
type countingProvider struct {
	stubProvider
	streams  int
	finished int
}

func (p *countingProvider) Generate(ctx context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	p.finished++
	return p.stubProvider.Generate(ctx, request)
}

func (p *countingProvider) GenerateStream(ctx context.Context, request inference.GenerateRequest) (inference.Stream, error) {
	p.streams++
	return p.stubProvider.GenerateStream(ctx, request)
}

func modelCall(t *testing.T, provider *countingProvider, config string) (graph.Result, []agent.Event) {
	t.Helper()
	rt := testRuntime(t, provider)
	snapshot := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "排下一位发言者"),
		})),
		"answer": graph.Text(""),
	}
	stream, err := nodes.Model(rt).Invoke(context.Background(), invokeFor(snapshot, config))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	return drainNode(t, stream)
}

func answerProvider() *countingProvider {
	return &countingProvider{stubProvider: stubProvider{response: inference.GenerateResponse{
		Message: message.NewTextMessage(message.RoleAssistant, "speaker=wukong; guidance=先别开门"),
	}}}
}

// TestModelStreamsByDefault: a spoken turn watches the text arrive.
func TestModelStreamsByDefault(t *testing.T) {
	provider := answerProvider()
	result, events := modelCall(t, provider, `{"messages":"messages","output":"answer"}`)
	if provider.streams != 1 || provider.finished != 0 {
		t.Fatalf("streams=%d finished=%d, want the streaming entry point", provider.streams, provider.finished)
	}
	if text := resultValue(t, result, "answer"); text != "speaker=wukong; guidance=先别开门" {
		t.Fatalf("answer = %q", text)
	}
	if len(events) == 0 {
		t.Fatal("a streaming call reported no events")
	}
}

// TestModelCanAnswerInOnePiece: a call that asked for a finished response uses the
// other entry point and still reports the round, then the whole answer once.
func TestModelCanAnswerInOnePiece(t *testing.T) {
	provider := answerProvider()
	result, events := modelCall(t, provider,
		`{"messages":"messages","output":"answer","request":{"stream":false}}`)
	if provider.finished != 1 || provider.streams != 0 {
		t.Fatalf("streams=%d finished=%d, want the finished entry point", provider.streams, provider.finished)
	}
	if text := resultValue(t, result, "answer"); text != "speaker=wukong; guidance=先别开门" {
		t.Fatalf("answer = %q", text)
	}
	var answered bool
	for _, event := range events {
		if typed, ok := event.(agent.Text); ok {
			answered = true
			if typed.Text != "speaker=wukong; guidance=先别开门" {
				t.Fatalf("text = %q, want the whole answer in one event", typed.Text)
			}
		}
	}
	if !answered {
		t.Fatal("the answer was not reported")
	}
}

// resultValue reads one written key out of a node result.
func resultValue(t *testing.T, result graph.Result, key string) string {
	t.Helper()
	for _, update := range result.State {
		if update.Key != key {
			continue
		}
		text, err := update.Value.Text()
		if err != nil {
			t.Fatalf("read %q: %v", key, err)
		}
		return text
	}
	t.Fatalf("no %q write", key)
	return ""
}
