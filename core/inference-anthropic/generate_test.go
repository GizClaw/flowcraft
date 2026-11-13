package anthropic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/goccy/go-json"
)

func testRequest(t *testing.T) inference.GenerateRequest {
	t.Helper()
	request := inference.GenerateRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "claude-test"}},
		Input: inference.GenerateInput{
			Role:    inference.InputRoleUser,
			Content: message.NewTextContent("hello"),
		},
		Output: inference.OutputText,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("request: %v", err)
	}
	return request
}

func testDriver(settings Settings) *driver {
	normalized := settings.normalized()
	return &driver{settings: normalized, client: newClient(normalized)}
}

func TestCompileMapsTextAndTools(t *testing.T) {
	definition := message.ToolDefinition{
		Name:        "search",
		Description: "search the corpus",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
	}
	tools, err := inference.NewToolset([]message.ToolDefinition{definition})
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}
	tools, err = tools.Choosing(inference.ToolChoice{Mode: inference.ToolChoiceRequired})
	if err != nil {
		t.Fatalf("Choosing: %v", err)
	}
	request := testRequest(t)
	request.Context = []message.Message{
		message.NewTextMessage(message.RoleSystem, "be brief"),
		message.NewTextMessage(message.RoleUser, "earlier"),
	}
	request.Tools = tools

	compiled, err := testDriver(Settings{APIKey: "key"}).compile(
		context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	wire := compiled.Wire
	if string(wire.Model) != "claude-test" || wire.MaxTokens != DefaultMaxTokens {
		t.Fatalf("model/max = %q/%d", wire.Model, wire.MaxTokens)
	}
	if len(wire.System) != 1 || wire.System[0].Text != "be brief" {
		t.Fatalf("system = %+v", wire.System)
	}
	if len(wire.Messages) != 1 || wire.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v", wire.Messages)
	}
	if len(wire.Messages[0].Content) != 2 {
		t.Fatalf("content blocks = %d, want two user blocks", len(wire.Messages[0].Content))
	}
	if len(wire.Tools) != 1 || wire.Tools[0].OfTool == nil || wire.Tools[0].OfTool.Name != "search" {
		t.Fatalf("tools = %+v", wire.Tools)
	}
	if wire.ToolChoice.OfAny == nil {
		t.Fatalf("tool choice = %+v, want required", wire.ToolChoice)
	}
}

func TestGenerateText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"msg_1","type":"message","role":"assistant","model":"claude-test",
			"content":[{"type":"text","text":"hello"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":10,"output_tokens":2,
				"cache_read_input_tokens":3,"cache_creation_input_tokens":1}
		}`)
	}))
	defer server.Close()

	provider, err := New(Settings{BaseURL: server.URL, APIKey: "key"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := testRequest(t)
	response, err := provider.Generate(context.Background(), request)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got := response.Message.Content.Text(); got != "hello" {
		t.Fatalf("text = %q", got)
	}
	if response.FinishReason != inference.FinishStop {
		t.Fatalf("finish = %s", response.FinishReason)
	}
	if response.Usage.InputTokens != 14 || response.Usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v", response.Usage)
	}
	if response.Usage.Input.CacheReadTokens == nil || *response.Usage.Input.CacheReadTokens != 3 {
		t.Fatalf("cache read = %v", response.Usage.Input.CacheReadTokens)
	}
	if response.Usage.Input.CacheWriteTokens == nil || *response.Usage.Input.CacheWriteTokens != 1 {
		t.Fatalf("cache write = %v", response.Usage.Input.CacheWriteTokens)
	}
	if response.Metadata.ResponseID != "msg_1" || response.Metadata.Model.Name != "claude-test" {
		t.Fatalf("metadata = %+v", response.Metadata)
	}
}

func TestGenerateToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"msg_2","type":"message","role":"assistant","model":"claude-test",
			"content":[{"type":"tool_use","id":"toolu_1","name":"search",
				"input":{"q":"x"}}],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":4,"output_tokens":1}
		}`)
	}))
	defer server.Close()

	provider, err := New(Settings{BaseURL: server.URL, APIKey: "key"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	definition := message.ToolDefinition{
		Name:        "search",
		Description: "search",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
	request := testRequest(t)
	request.Tools, _ = inference.NewToolset([]message.ToolDefinition{definition})
	response, err := provider.Generate(context.Background(), request)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.FinishReason != inference.FinishToolCalls {
		t.Fatalf("finish = %s", response.FinishReason)
	}
	calls := response.Message.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "search" || string(calls[0].Arguments.Raw()) != `{"q":"x"}` {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestGenerateClassifiesRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`)
	}))
	defer server.Close()

	provider, err := New(Settings{BaseURL: server.URL, APIKey: "key"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.Generate(context.Background(), testRequest(t))
	if err == nil || !inference.IsKind(err, inference.ProviderFailure) {
		t.Fatalf("error = %v, want ProviderFailure", err)
	}
	if got := asInferenceError(t, err).RetryAfter; got != 2*1e9 {
		t.Fatalf("retry after = %v", got)
	}
}

func asInferenceError(t *testing.T, err error) *inference.Error {
	t.Helper()
	var target *inference.Error
	if !errors.As(err, &target) {
		t.Fatalf("not an inference error: %v", err)
	}
	return target
}

func TestCompileMapsImageAndDocument(t *testing.T) {
	image, err := media.NewImageURL("https://example.test/image.png", "image/png")
	if err != nil {
		t.Fatalf("image: %v", err)
	}
	document, err := media.NewFileURL("https://example.test/report.pdf", "application/pdf")
	if err != nil {
		t.Fatalf("document: %v", err)
	}
	request := testRequest(t)
	request.Input.Content = message.Content{Parts: []message.Part{
		message.TextPart{Text: "look"},
		message.ImagePart{Source: image},
		message.FilePart{Source: document, Name: "report.pdf"},
	}}
	compiled, err := testDriver(Settings{APIKey: "key"}).compile(
		context.Background(), request, protocol.ShapeUnary)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	blocks := compiled.Wire.Messages[0].Content
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want three", len(blocks))
	}
	if blocks[1].OfImage == nil || blocks[1].OfImage.Source.OfURL == nil {
		t.Fatalf("image block = %+v", blocks[1])
	}
	if blocks[2].OfDocument == nil || blocks[2].OfDocument.Source.OfURL == nil {
		t.Fatalf("document block = %+v", blocks[2])
	}
}

func TestGenerateStreamText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"usage":{"input_tokens":4,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`)
	}))
	defer server.Close()

	provider, err := New(Settings{BaseURL: server.URL, APIKey: "key"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := provider.GenerateStream(context.Background(), testRequest(t))
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	var text string
	var started bool
	for {
		event, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch event.Kind {
		case inference.StreamStart:
			started = true
		case inference.StreamText:
			text += event.Text
		}
	}
	if !started || text != "hello" {
		t.Fatalf("started=%v text=%q", started, text)
	}
	response, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if response.FinishReason != inference.FinishStop {
		t.Fatalf("finish = %s", response.FinishReason)
	}
	if response.Usage.InputTokens != 4 || response.Usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v", response.Usage)
	}
}
