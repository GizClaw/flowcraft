package openai

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/resource"
)

// scriptedClients points the driver's clients at the shared scripted wire
// server instead of a hand-rolled httptest handler.
func scriptedClients(t *testing.T, srv *inferencetest.ScriptedOpenAI) *clients {
	t.Helper()
	spec, err := decodeSpec(context.Background(), []byte(
		fmt.Sprintf(`{"endpoint":{"base_url":%q}}`, srv.URL()),
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	cls, err := profileMaterial{
		apiKey: resource.LiteralSecret("test-key"),
	}.newClients(context.Background(), spec)
	if err != nil {
		t.Fatalf("newClients: %v", err)
	}
	return cls
}

// The two tests below are the pilot for the shared scripted provider
// (inferencetest.ScriptedOpenAI): one unary and one streamed chat round
// trip through the real wire bytes. The hand-rolled capturedOpenAI
// fixtures retire at their own pace; this pair pins that the shared
// server is a drop-in replacement, request assertions included.

func TestChatScriptedWireUnary(t *testing.T) {
	srv := inferencetest.NewScriptedOpenAI(t,
		inferencetest.ScriptedReply{Text: "hello from scripted"},
	)
	request, err := compileChatFor(
		"gpt-5.6-sol", testTargetWith("gpt-5.6-sol", chatWire()),
	)(context.Background(), openaiModel("gpt-5.6-sol"), simpleTextRequest("hi"), inference.GenerateExecutionUnary)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := transportChatGenerate(scriptedClients(t, srv).api)(
		context.Background(), request.Wire)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	response, err := decodeGenerate(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if response.FinishReason != inference.FinishCompleted ||
		response.Message.Content.Text() != "hello from scripted" {
		t.Fatalf("response = %+v", response)
	}
	if response.Usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v, want the scripted 15 total", response.Usage)
	}
	if got := srv.Calls(); got != 1 {
		t.Fatalf("scripted provider saw %d calls, want 1", got)
	}
	messages, err := srv.LastMessages()
	if err != nil {
		t.Fatalf("LastMessages: %v", err)
	}
	if !hasUserTurn(messages, "hi") {
		t.Fatalf("scripted provider saw %v, want the user turn", messages)
	}
}

func TestChatScriptedWireStream(t *testing.T) {
	srv := inferencetest.NewScriptedOpenAI(t,
		inferencetest.ScriptedReply{Text: "hello stream", RequestID: "req_scripted_1"},
	)
	request, err := compileChatFor(
		"gpt-5.6-sol", testTargetWith("gpt-5.6-sol", chatWire()),
	)(context.Background(), openaiModel("gpt-5.6-sol"), simpleTextRequest("hi"), inference.GenerateExecutionStream)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := transportChatGenerateStream(scriptedClients(t, srv).api)(
		context.Background(), request.Wire)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	meta, ok := stream.(inference.ProviderStreamMetadata)
	if !ok {
		t.Fatal("chat stream must expose provider stream metadata")
	}
	if got := meta.RequestID(); got != "req_scripted_1" {
		t.Fatalf("stream request id = %q, want req_scripted_1", got)
	}

	var text string
	var finish inference.FinishReason
	var usage *inference.Usage
	for {
		raw, err := stream.Next(context.Background())
		if err != nil {
			break
		}
		event, err := decodeChatGenerateStream(context.Background(), raw)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if delta, ok := event.Delta.(inference.TextPartDelta); ok {
			text += delta.Text
		}
		if event.FinishReason != "" {
			finish = event.FinishReason
		}
		if event.Usage != nil {
			usage = event.Usage
		}
	}
	if text != "hello stream" || finish != inference.FinishCompleted ||
		usage == nil || usage.TotalTokens != 15 {
		t.Fatalf("stream text=%q finish=%q usage=%+v", text, finish, usage)
	}
	if got := srv.Calls(); got != 1 {
		t.Fatalf("scripted provider saw %d calls, want 1", got)
	}
}

func hasUserTurn(messages []map[string]any, text string) bool {
	for _, msg := range messages {
		if msg["role"] == "user" && fmt.Sprint(msg["content"]) == text {
			return true
		}
	}
	return false
}
