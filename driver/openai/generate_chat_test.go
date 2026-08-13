package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

func TestChatCompileRejectsWebSearch(t *testing.T) {
	request := simpleTextRequest("hi")
	request.Extensions = inference.Extensions{
		GenerateOptions{WebSearch: &GenerateWebSearch{}},
	}
	compiled, err := compileGenerate("gpt-5.6-sol", catalogEntry{
		kind: kindGenerate, api: apiChat, webSearch: true,
	})(context.Background(), openaiModel("gpt-5.6-sol"), request, inference.GenerateExecutionUnary)
	if err == nil || !strings.Contains(err.Error(), "web_search") {
		t.Fatalf("compile error = %v, want web_search rejection", err)
	}
	if compiled.Wire.webSearch != nil {
		t.Fatal("chat wire carries web_search")
	}
}

func TestChatUnaryTransportAndDecode(t *testing.T) {
	server, capture := newCapturedOpenAI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"id": "chatcmpl_1",
			"object": "chat.completion",
			"choices": [{
				"index": 0,
				"message": {"role": "assistant", "content": "ok"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3}
		}`)
	})
	defer server.Close()

	wire, err := compileGenerate("gpt-5.6-sol", catalogEntry{
		kind: kindGenerate, api: apiChat,
	})(context.Background(), openaiModel("gpt-5.6-sol"), simpleTextRequest("hi"), inference.GenerateExecutionUnary)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := transportChatGenerate(testClients(t, server).api)(context.Background(), wire.Wire)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	response, err := decodeGenerate(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if response.FinishReason != inference.FinishCompleted ||
		len(response.Message.Content.Parts) != 1 ||
		response.Message.Content.Text() != "ok" ||
		response.Usage.TotalTokens != 3 {
		t.Fatalf("response = %+v", response)
	}
	body := capture.body(0)
	if body["model"] != "gpt-5.6-sol" {
		t.Fatalf("model = %v", body["model"])
	}
}

func TestChatStreamTransportAndDecode(t *testing.T) {
	server, _ := newCapturedOpenAI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, strings.Join([]string{
			`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"he"},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
			"data: [DONE]",
		}, "\n\n")+"\n\n")
	})
	defer server.Close()

	wire, err := compileGenerate("gpt-5.6-sol", catalogEntry{
		kind: kindGenerate, api: apiChat,
	})(context.Background(), openaiModel("gpt-5.6-sol"), simpleTextRequest("hi"), inference.GenerateExecutionStream)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := transportChatGenerateStream(testClients(t, server).api)(
		context.Background(), wire.Wire)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

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
	if text != "hello" || finish != inference.FinishCompleted || usage == nil || usage.TotalTokens != 3 {
		t.Fatalf("stream text=%q finish=%q usage=%+v", text, finish, usage)
	}
}

func TestSpecAPIFieldValidation(t *testing.T) {
	if _, err := decodeSpec([]byte(`{"api":"chat"}`)); err != nil {
		t.Fatalf("chat api rejected: %v", err)
	}
	if _, err := decodeSpec([]byte(`{"api":"bogus"}`)); err == nil {
		t.Fatal("bogus api accepted")
	}
}

var _ = json.RawMessage{}
