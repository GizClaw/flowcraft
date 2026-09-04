package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"

	"github.com/openai/openai-go/v3"
)

func boolPtr(value bool) *bool {
	return &value
}

func TestChatCompileRejectsWebSearch(t *testing.T) {
	request := simpleTextRequest("hi")
	request.Extensions = inference.Extensions{
		GenerateOptions{WebSearch: &GenerateWebSearch{}},
	}
	compiled, err := compileGenerate("gpt-5.6-sol", catalogEntry{
		kind:         kindGenerate,
		api:          apiChat,
		capabilities: inference.ModelCapabilities{HostedWebSearch: true},
	})(context.Background(), openaiModel("gpt-5.6-sol"), request, inference.GenerateExecutionUnary)
	if err == nil || !strings.Contains(err.Error(), "web_search") {
		t.Fatalf("compile error = %v, want web_search rejection", err)
	}
	if compiled.Wire.webSearch != nil {
		t.Fatal("chat wire carries web_search")
	}
}

func TestChatUsageMapsProviderDetails(t *testing.T) {
	var wireUsage openai.CompletionUsage
	if err := json.Unmarshal([]byte(`{
		"prompt_tokens": 10,
		"completion_tokens": 8,
		"total_tokens": 18,
		"prompt_tokens_details": {
			"cached_tokens": 3,
			"cache_write_tokens": 4,
			"audio_tokens": 2
		},
		"completion_tokens_details": {
			"reasoning_tokens": 5,
			"accepted_prediction_tokens": 1,
			"rejected_prediction_tokens": 1,
			"audio_tokens": 2
		}
	}`), &wireUsage); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	usage := rawUsageCanonical(chatUsageToRaw(wireUsage))

	if usage.Input.CacheReadTokens == nil || *usage.Input.CacheReadTokens != 3 {
		t.Fatalf("cache read = %+v", usage.Input)
	}
	if usage.Input.CacheWriteTokens == nil || *usage.Input.CacheWriteTokens != 4 {
		t.Fatalf("cache write = %+v", usage.Input)
	}
	if usage.Output.ReasoningTokens == nil || *usage.Output.ReasoningTokens != 5 {
		t.Fatalf("reasoning = %+v", usage.Output)
	}
	if usage.Output.ReasoningAccounting != inference.ReasoningIncludedInOutput {
		t.Fatalf(
			"reasoning accounting = %q, want included_in_output",
			usage.Output.ReasoningAccounting,
		)
	}
	if usage.Output.AcceptedPredictionTokens == nil ||
		*usage.Output.AcceptedPredictionTokens != 1 {
		t.Fatalf("accepted predictions = %+v", usage.Output)
	}
	if usage.Output.RejectedPredictionTokens == nil ||
		*usage.Output.RejectedPredictionTokens != 1 {
		t.Fatalf("rejected predictions = %+v", usage.Output)
	}
	if got := modalityTokens(usage.Input.ByModality); got != 2 {
		t.Fatalf("input audio tokens = %d, want 2", got)
	}
	if got := modalityTokens(usage.Output.ByModality); got != 2 {
		t.Fatalf("output audio tokens = %d, want 2", got)
	}
}

func modalityTokens(usages []inference.ModalityTokenUsage) int64 {
	for _, usage := range usages {
		if usage.Modality == inference.ModalityAudio {
			return usage.Tokens
		}
	}
	return 0
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
	server, capture := newCapturedOpenAI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
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
	streamOptions, ok := capture.body(0)["stream_options"].(map[string]any)
	if !ok {
		t.Fatal("chat stream request must default to stream_options")
	}
	if includeUsage, ok := streamOptions["include_usage"].(bool); !ok || !includeUsage {
		t.Fatalf("stream_options = %v, want include_usage=true", streamOptions)
	}
	if _, ok := streamOptions["include_obfuscation"]; ok {
		t.Fatalf("default stream_options must not set include_obfuscation: %v", streamOptions)
	}
}

func TestChatStreamUsageOptOutOmitsStreamOptions(t *testing.T) {
	server, capture := newCapturedOpenAI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, strings.Join([]string{
			`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			"data: [DONE]",
		}, "\n\n")+"\n\n")
	})
	defer server.Close()

	entry := catalogEntry{
		kind:                   kindGenerate,
		api:                    apiChat,
		capabilities:           generateChatCapabilities(),
		chatStreamIncludeUsage: boolPtr(false),
	}
	wire, err := compileGenerate("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		simpleTextRequest("hi"),
		inference.GenerateExecutionStream,
	)
	if err != nil {
		t.Fatal(err)
	}
	if wire.Wire.chatStreamIncludeUsage {
		t.Fatal("compiled wire must carry the usage opt-out")
	}
	stream, err := transportChatGenerateStream(testClients(t, server).api)(
		context.Background(), wire.Wire)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	for {
		if _, err := stream.Next(context.Background()); err != nil {
			break
		}
	}
	if _, ok := capture.body(0)["stream_options"]; ok {
		t.Fatal("chat stream request must omit stream_options when usage opt-out is set")
	}
}

func TestChatStreamObfuscationOptOutSendsStreamOptions(t *testing.T) {
	cases := []struct {
		name         string
		includeUsage *bool
		wantUsage    bool
	}{
		{name: "keeps default usage", wantUsage: true},
		{name: "usage opt out", includeUsage: boolPtr(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, capture := newCapturedOpenAI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, strings.Join([]string{
					`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
					`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
					"data: [DONE]",
				}, "\n\n")+"\n\n")
			})
			defer server.Close()

			entry := catalogEntry{
				kind:                         kindGenerate,
				api:                          apiChat,
				capabilities:                 generateChatCapabilities(),
				chatStreamIncludeUsage:       tc.includeUsage,
				chatStreamIncludeObfuscation: boolPtr(false),
			}
			wire, err := compileGenerate("gpt-5.6-sol", entry)(
				context.Background(),
				openaiModel("gpt-5.6-sol"),
				simpleTextRequest("hi"),
				inference.GenerateExecutionStream,
			)
			if err != nil {
				t.Fatal(err)
			}
			stream, err := transportChatGenerateStream(testClients(t, server).api)(
				context.Background(), wire.Wire)
			if err != nil {
				t.Fatalf("open stream: %v", err)
			}
			defer func() { _ = stream.Close() }()
			for {
				if _, err := stream.Next(context.Background()); err != nil {
					break
				}
			}
			streamOptions, ok := capture.body(0)["stream_options"].(map[string]any)
			if !ok {
				t.Fatal("explicit include_obfuscation=false must keep stream_options on the wire")
			}
			if tc.wantUsage {
				if includeUsage, ok := streamOptions["include_usage"].(bool); !ok || !includeUsage {
					t.Fatalf("stream_options = %v, want include_usage=true", streamOptions)
				}
			} else if _, ok := streamOptions["include_usage"]; ok {
				t.Fatalf("stream_options = %v, want no include_usage", streamOptions)
			}
			if includeObfuscation, ok := streamOptions["include_obfuscation"].(bool); !ok || includeObfuscation {
				t.Fatalf("stream_options = %v, want include_obfuscation=false", streamOptions)
			}
		})
	}
}

func TestSpecAPIFieldValidation(t *testing.T) {
	if _, err := decodeSpec(context.Background(), []byte(`{"api":"chat"}`)); err != nil {
		t.Fatalf("chat api rejected: %v", err)
	}
	if _, err := decodeSpec(context.Background(), []byte(`{"api":"bogus"}`)); err == nil {
		t.Fatal("bogus api accepted")
	}
}

var _ = json.RawMessage{}
