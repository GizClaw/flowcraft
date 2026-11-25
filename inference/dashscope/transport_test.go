package dashscope

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/outcome"
)

// The tests below drive the whole pipeline through a real HTTP server: compile,
// transport and decode all run, so the wire shapes the platform actually speaks are
// pinned rather than only the ones a fake transport was told about.

func textModelSpec() ModelSpec {
	declared := ModelSpec{
		Name: "qwen-test",
		Kind: "generate",
		Capabilities: model.ModelCapabilities{
			Inputs: []message.PartKind{message.PartText},
			Modes:  []model.Mode{model.ModeText},
		},
	}
	declared.Generate.API = generateAPIMultimodal
	return declared
}

func embedModelSpec() ModelSpec {
	declared := ModelSpec{
		Name: "embed-test",
		Kind: "embed",
		Capabilities: model.ModelCapabilities{
			Inputs: []message.PartKind{message.PartText},
		},
	}
	declared.Embed = EmbedParams{BatchMax: 2}
	return declared
}

func TestGenerateRoundTripsThroughHTTP(t *testing.T) {
	server := newJSONServer(t, func(w io.Writer) {
		_, _ = io.WriteString(w, `{
			"output": {"choices": [{"message": {"content": "hi there"}, "finish_reason": "stop"}]},
			"usage": {"input_tokens": 3, "output_tokens": 2},
			"request_id": "req-1"
		}`)
	})

	provider, err := New(Spec{APIKey: "key", BaseURL: server, Models: []ModelSpec{textModelSpec()}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	request := inference.GenerateRequest{
		Model:  model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "qwen-test"}},
		Input:  inference.GenerateInput{Role: inference.InputRoleUser, Content: message.NewTextContent("hello")},
		Output: inference.OutputText,
	}
	response, err := provider.Generate(context.Background(), request)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got := response.Message.Content.Text(); got != "hi there" {
		t.Fatalf("text = %q", got)
	}
	if response.FinishReason != inference.FinishStop {
		t.Fatalf("finish = %q", response.FinishReason)
	}
	if response.Usage.OutputTokens != 2 || response.Metadata.RequestID != "req-1" {
		t.Fatalf("usage/metadata = %+v / %+v", response.Usage, response.Metadata)
	}
}

func TestGenerateDecodesToolCalls(t *testing.T) {
	server := newJSONServer(t, func(w io.Writer) {
		_, _ = io.WriteString(w, `{
			"output": {"choices": [{
				"message": {"content": "", "tool_calls": [{"id": "call-1", "function": {"name": "search", "arguments": "{\"q\":\"x\"}"}}]},
				"finish_reason": "tool_calls"
			}]},
			"request_id": "req-2"
		}`)
	})
	provider, err := New(Spec{APIKey: "key", BaseURL: server, Models: []ModelSpec{textModelSpec()}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tools, err := inference.NewToolset([]message.ToolDefinition{{
		Name:        "search",
		InputSchema: []byte(`{"type":"object"}`),
	}})
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}
	request := inference.GenerateRequest{
		Model:  model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "qwen-test"}},
		Input:  inference.GenerateInput{Role: inference.InputRoleUser, Content: message.NewTextContent("find x")},
		Output: inference.OutputText,
		Tools:  tools,
	}
	response, err := provider.Generate(context.Background(), request)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.FinishReason != inference.FinishToolCalls {
		t.Fatalf("finish = %q", response.FinishReason)
	}
	var call *message.ToolCallPart
	for _, part := range response.Message.Content.Parts {
		if typed, ok := part.(message.ToolCallPart); ok {
			call = &typed
		}
	}
	if call == nil || call.Call.Name != "search" {
		t.Fatalf("parts = %+v", response.Message.Content.Parts)
	}
}

// TestGenerateClassifiesTheEnvelope drives a failure the way the platform reports
// one: HTTP 200 or 4xx with a code and message body.
func TestGenerateClassifiesTheEnvelope(t *testing.T) {
	server := newJSONServerStatus(t, 403, func(w io.Writer) {
		_, _ = io.WriteString(w, `{"code":"InvalidApiKey","message":"bad key","request_id":"req-3"}`)
	})
	provider, err := New(Spec{APIKey: "key", BaseURL: server, Models: []ModelSpec{textModelSpec()}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := inference.GenerateRequest{
		Model:  model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "qwen-test"}},
		Input:  inference.GenerateInput{Role: inference.InputRoleUser, Content: message.NewTextContent("hello")},
		Output: inference.OutputText,
	}
	if _, err := provider.Generate(context.Background(), request); err == nil {
		t.Fatal("Generate accepted a 403")
	}
}

func TestEmbedRoundTripsThroughHTTP(t *testing.T) {
	server := newJSONServer(t, func(w io.Writer) {
		_, _ = io.WriteString(w, `{
			"output": {"embeddings": [
				{"text_index": 0, "embedding": [0.1, 0.2]},
				{"text_index": 1, "embedding": [0.3, 0.4]}
			]},
			"usage": {"total_tokens": 4},
			"request_id": "req-4"
		}`)
	})
	provider, err := New(Spec{APIKey: "key", BaseURL: server, Models: []ModelSpec{embedModelSpec()}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if provider.Embedder == nil {
		t.Fatal("the provider serves no embeddings")
	}

	request := inference.EmbedRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "embed-test"}},
		Input: []message.Content{message.NewTextContent("a"), message.NewTextContent("b")},
	}
	response, err := provider.Embedder.Embed(context.Background(), request)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(response.Vectors) != 2 || len(response.Vectors[0]) != 2 {
		t.Fatalf("vectors = %+v", response.Vectors)
	}
	if response.Usage.InputTokens != 4 {
		t.Fatalf("usage = %+v", response.Usage)
	}
}

// TestGenerateStreamDecodesSSE covers the streaming transport: the platform answers
// with server-sent events, and each data line is one delta.
func TestGenerateStreamDecodesSSE(t *testing.T) {
	server := newSSEServer(t, []string{
		`{"output":{"choices":[{"message":{"content":"he"}}]},"request_id":"req-5"}`,
		`{"output":{"choices":[{"message":{"content":"llo"},"finish_reason":"stop"}],"usage":{"input_tokens":1,"output_tokens":2}}}`,
	})
	provider, err := New(Spec{APIKey: "key", BaseURL: server, Models: []ModelSpec{textModelSpec()}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if provider.Generator == nil {
		t.Fatal("the provider serves no generation")
	}

	request := inference.GenerateRequest{
		Model:  model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "qwen-test"}},
		Input:  inference.GenerateInput{Role: inference.InputRoleUser, Content: message.NewTextContent("hi")},
		Output: inference.OutputText,
	}
	stream, err := provider.GenerateStream(context.Background(), request)
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var text string
	for {
		event, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		text += event.Text
	}
	if text != "hello" {
		t.Fatalf("streamed text = %q", text)
	}
}

// TestImageTaskRoundTrip covers the asynchronous surface: create a task, poll it to
// a terminal state, then read the links it names. The task lifecycle is where a
// media backend spends its calls, so the states are pinned here.
func TestImageTaskRoundTrip(t *testing.T) {
	var base string
	queries := 0
	server := newTaskServer(t, func(w io.Writer, path string) bool {
		switch {
		case path == imagePathText2Image:
			_, _ = io.WriteString(w, `{"output":{"task_id":"task-1"},"request_id":"req-6"}`)
		case strings.HasPrefix(path, taskPathPrefix):
			queries++
			if queries == 1 {
				_, _ = io.WriteString(w, `{"output":{"task_status":"RUNNING"},"request_id":"req-6"}`)
				return true
			}
			_, _ = io.WriteString(w, `{"output":{"task_status":"SUCCEEDED","results":[{"url":"`+base+`/result/1.png"}]},"request_id":"req-6"}`)
		case path == "/result/1.png":
			_, _ = io.WriteString(w, "PNG")
		default:
			return false
		}
		return true
	})
	base = server

	declared := ModelSpec{
		Name: "image-test",
		Kind: "image",
		Capabilities: model.ModelCapabilities{
			Inputs: []message.PartKind{message.PartText},
			Modes:  []model.Mode{model.ModeImage},
		},
	}
	declared.Image = ImageParams{Format: media.ImageFormatPNG}

	provider, err := New(Spec{
		APIKey:             "key",
		BaseURL:            server,
		PollIntervalMillis: 1,
		Models:             []ModelSpec{declared},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	request := inference.GenerateRequest{
		Model:  model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "image-test"}},
		Input:  inference.GenerateInput{Role: inference.InputRoleUser, Content: message.NewTextContent("a cat")},
		Output: inference.OutputImage,
	}
	// The poll loop ends on the caller's context, so a handler that never reaches a
	// terminal state must fail the test rather than hang the suite.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := provider.Generate(ctx, request)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(response.Message.Content.Parts) != 1 {
		t.Fatalf("parts = %+v", response.Message.Content.Parts)
	}
	image, ok := response.Message.Content.Parts[0].(message.ImagePart)
	if !ok {
		t.Fatalf("part = %T, want an image", response.Message.Content.Parts[0])
	}
	if got := image.Source.URL(); got != server+"/result/1.png" {
		t.Fatalf("image url = %q", got)
	}
	if got := response.Usage.Work[outcome.WorkImagesGenerated]; got != 1 {
		t.Fatalf("work accounting = %v", response.Usage.Work)
	}
}

// TestImageTaskReportsAFailedState keeps a terminal failure from looking like an
// empty success: the platform's own code and message reach the caller.
func TestImageTaskReportsAFailedState(t *testing.T) {
	server := newTaskServer(t, func(w io.Writer, path string) bool {
		switch {
		case path == imagePathText2Image:
			_, _ = io.WriteString(w, `{"output":{"task_id":"task-2"},"request_id":"req-7"}`)
		case strings.HasPrefix(path, taskPathPrefix):
			_, _ = io.WriteString(w, `{"output":{"task_status":"FAILED","code":"DataInspectionFailed","message":"bad prompt"}}`)
		default:
			return false
		}
		return true
	})

	declared := ModelSpec{
		Name: "image-test", Kind: "image",
		Capabilities: model.ModelCapabilities{
			Inputs: []message.PartKind{message.PartText},
			Modes:  []model.Mode{model.ModeImage},
		},
	}
	declared.Image = ImageParams{Format: media.ImageFormatPNG}
	provider, err := New(Spec{APIKey: "key", BaseURL: server, PollIntervalMillis: 1, Models: []ModelSpec{declared}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := inference.GenerateRequest{
		Model:  model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "image-test"}},
		Input:  inference.GenerateInput{Role: inference.InputRoleUser, Content: message.NewTextContent("a cat")},
		Output: inference.OutputImage,
	}
	if _, err := provider.Generate(context.Background(), request); err == nil {
		t.Fatal("Generate accepted a failed task")
	}
}
