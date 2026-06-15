package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

func mustConvertMessages(t *testing.T, msgs []llm.Message) []chatMessage {
	t.Helper()
	out, err := convertMessages(msgs)
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	return out
}

func TestConvertMessages_TextOnly(t *testing.T) {
	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, "You are a helper."),
		llm.NewTextMessage(llm.RoleUser, "Hello"),
		llm.NewTextMessage(llm.RoleAssistant, "Hi there!"),
	}

	out := mustConvertMessages(t, msgs)
	if len(out) != 3 {
		t.Fatalf("got %d messages, want 3", len(out))
	}
	if out[0].Role != "system" || out[0].Content != "You are a helper." {
		t.Errorf("system msg = %+v", out[0])
	}
	if out[1].Role != "user" || out[1].Content != "Hello" {
		t.Errorf("user msg = %+v", out[1])
	}
	if out[2].Role != "assistant" || out[2].Content != "Hi there!" {
		t.Errorf("assistant msg = %+v", out[2])
	}
}

func TestConvertMessages_WithToolCalls(t *testing.T) {
	msg := llm.NewToolCallMessage([]llm.ToolCall{
		{ID: "call_1", Name: "get_weather", Arguments: `{"city":"NYC"}`},
	})

	out := mustConvertMessages(t, []llm.Message{msg})
	if len(out) != 1 {
		t.Fatalf("got %d messages, want 1", len(out))
	}
	if out[0].Role != "assistant" {
		t.Errorf("role = %q, want assistant", out[0].Role)
	}
	if len(out[0].ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(out[0].ToolCalls))
	}
	tc := out[0].ToolCalls[0]
	if tc.Function.Name != "get_weather" {
		t.Errorf("tool name = %q", tc.Function.Name)
	}
	if tc.Function.Arguments["city"] != "NYC" {
		t.Errorf("tool args = %v", tc.Function.Arguments)
	}
}

func TestConvertMessages_ToolCallInvalidJSON(t *testing.T) {
	msg := llm.NewToolCallMessage([]llm.ToolCall{
		{ID: "call_1", Name: "do_thing", Arguments: `not valid json`},
	})

	out := mustConvertMessages(t, []llm.Message{msg})
	tc := out[0].ToolCalls[0]
	if tc.Function.Arguments["_raw"] != "not valid json" {
		t.Errorf("expected _raw fallback, got %v", tc.Function.Arguments)
	}
}

func TestConvertMessages_ToolResults(t *testing.T) {
	msg := llm.NewToolResultMessage([]llm.ToolResult{
		{ToolCallID: "call_1", Content: "sunny, 72F"},
		{ToolCallID: "call_2", Content: "rainy, 55F"},
	})

	out := mustConvertMessages(t, []llm.Message{msg})
	if len(out) != 2 {
		t.Fatalf("tool results should expand to %d messages, got %d", 2, len(out))
	}
	for _, m := range out {
		if m.Role != "tool" {
			t.Errorf("role = %q, want tool", m.Role)
		}
	}
	if out[0].Content != "sunny, 72F" {
		t.Errorf("first result = %q", out[0].Content)
	}
}

func TestConvertMessages_ImageParts(t *testing.T) {
	msg := llm.Message{
		Role: llm.RoleUser,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "What is this?"},
			{Type: llm.PartImage, Image: &llm.MediaRef{URL: "data:image/png;base64,iVBORw0KGgo="}},
		},
	}

	out := mustConvertMessages(t, []llm.Message{msg})
	if len(out) != 1 {
		t.Fatalf("got %d messages", len(out))
	}
	if out[0].Content != "What is this?" {
		t.Errorf("content = %q", out[0].Content)
	}
	if len(out[0].Images) != 1 {
		t.Fatalf("got %d images, want 1", len(out[0].Images))
	}
	if out[0].Images[0] != "iVBORw0KGgo=" {
		t.Errorf("image = %q", out[0].Images[0])
	}
}

func TestConvertMessages_EmptyImageSkipped(t *testing.T) {
	msg := llm.Message{
		Role: llm.RoleUser,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "hi"},
			{Type: llm.PartImage, Image: &llm.MediaRef{URL: ""}},
			{Type: llm.PartImage, Image: nil},
		},
	}

	out := mustConvertMessages(t, []llm.Message{msg})
	if len(out[0].Images) != 0 {
		t.Errorf("expected no images, got %d", len(out[0].Images))
	}
}

func TestConvertOllamaResponse_TextOnly(t *testing.T) {
	msg := convertOllamaResponse(chatMessage{
		Role:    "assistant",
		Content: "Hello!",
	})

	if msg.Role != llm.RoleAssistant {
		t.Errorf("role = %q", msg.Role)
	}
	if msg.Content() != "Hello!" {
		t.Errorf("content = %q", msg.Content())
	}
	if msg.HasToolCalls() {
		t.Error("unexpected tool calls")
	}
}

func TestConvertOllamaResponse_WithToolCalls(t *testing.T) {
	msg := convertOllamaResponse(chatMessage{
		Role:    "assistant",
		Content: "Let me check.",
		ToolCalls: []ollamaToolCall{
			{Function: ollamaFunctionCall{Name: "search", Arguments: map[string]any{"q": "test"}}},
			{Function: ollamaFunctionCall{Name: "calc", Arguments: map[string]any{"expr": "2+2"}}},
		},
	})

	if msg.Role != llm.RoleAssistant {
		t.Errorf("role = %q", msg.Role)
	}
	if msg.Content() != "Let me check." {
		t.Errorf("content = %q", msg.Content())
	}

	calls := msg.ToolCalls()
	if len(calls) != 2 {
		t.Fatalf("got %d tool calls, want 2", len(calls))
	}

	if calls[0].ID != "call_0" {
		t.Errorf("first call ID = %q, want call_0", calls[0].ID)
	}
	if calls[0].Name != "search" {
		t.Errorf("first call name = %q", calls[0].Name)
	}
	if calls[1].ID != "call_1" {
		t.Errorf("second call ID = %q, want call_1", calls[1].ID)
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	if args["q"] != "test" {
		t.Errorf("args = %v", args)
	}
}

func TestConvertOllamaResponse_ToolCallsNoText(t *testing.T) {
	msg := convertOllamaResponse(chatMessage{
		Role:    "assistant",
		Content: "",
		ToolCalls: []ollamaToolCall{
			{Function: ollamaFunctionCall{Name: "do_thing", Arguments: map[string]any{}}},
		},
	})

	if msg.Content() != "" {
		t.Errorf("expected empty content, got %q", msg.Content())
	}
	if len(msg.ToolCalls()) != 1 {
		t.Fatalf("expected 1 tool call")
	}
}

func TestApplyGenerateOptions_AllFields(t *testing.T) {
	temp := 0.7
	topP := 0.9
	topK := int64(40)
	maxTok := int64(1024)
	freqP := 0.5
	presP := 0.3
	jsonMode := true

	opts := &llm.GenerateOptions{
		Temperature:      &temp,
		TopP:             &topP,
		TopK:             &topK,
		MaxTokens:        &maxTok,
		StopWords:        []string{"END", "STOP"},
		FrequencyPenalty: &freqP,
		PresencePenalty:  &presP,
		JSONMode:         &jsonMode,
		Tools: []llm.ToolDefinition{
			{Name: "get_time", Description: "Get current time", InputSchema: map[string]any{"type": "object"}},
		},
	}

	req := &chatRequest{Model: "llama3"}
	applyGenerateOptions(req, opts)

	if req.Options == nil {
		t.Fatal("expected options to be set")
	}
	if *req.Options.Temperature != 0.7 {
		t.Errorf("temperature = %v", *req.Options.Temperature)
	}
	if *req.Options.TopP != 0.9 {
		t.Errorf("topP = %v", *req.Options.TopP)
	}
	if *req.Options.TopK != 40 {
		t.Errorf("topK = %v", *req.Options.TopK)
	}
	if *req.Options.NumPredict != 1024 {
		t.Errorf("numPredict = %v", *req.Options.NumPredict)
	}
	if len(req.Options.Stop) != 2 {
		t.Errorf("stop = %v", req.Options.Stop)
	}
	if *req.Options.Frequency != 0.5 {
		t.Errorf("frequency = %v", *req.Options.Frequency)
	}
	if *req.Options.Presence != 0.3 {
		t.Errorf("presence = %v", *req.Options.Presence)
	}
	if req.Format != "json" {
		t.Errorf("format = %v, want json", req.Format)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(req.Tools))
	}
	if req.Tools[0].Function.Name != "get_time" {
		t.Errorf("tool name = %q", req.Tools[0].Function.Name)
	}
}

func TestApplyGenerateOptions_NoOptions(t *testing.T) {
	opts := &llm.GenerateOptions{}
	req := &chatRequest{Model: "llama3"}
	applyGenerateOptions(req, opts)

	if req.Options != nil {
		t.Error("expected nil options when nothing is set")
	}
	if req.Format != nil {
		t.Error("expected nil format")
	}
}

func TestApplyGenerateOptions_JSONModeFalse(t *testing.T) {
	jsonMode := false
	opts := &llm.GenerateOptions{JSONMode: &jsonMode}
	req := &chatRequest{Model: "llama3"}
	applyGenerateOptions(req, opts)

	if req.Format != nil {
		t.Error("expected nil format when JSONMode is false")
	}
}

func TestNew_DefaultBaseURL(t *testing.T) {
	l, err := New("llama3", "")
	if err != nil {
		t.Fatal(err)
	}
	if l.baseURL != defaultBaseURL {
		t.Errorf("baseURL = %q, want %q", l.baseURL, defaultBaseURL)
	}
}

func TestNew_CustomBaseURL(t *testing.T) {
	l, err := New("llama3", "  http://myhost:8080/  ")
	if err != nil {
		t.Fatal(err)
	}
	if l.baseURL != "http://myhost:8080" {
		t.Errorf("baseURL = %q", l.baseURL)
	}
}

func TestNormalizeImageToBase64(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "data URI",
			input: "data:image/png;base64,iVBORw0KGgo=",
			want:  "iVBORw0KGgo=",
		},
		{
			name:  "plain base64",
			input: "iVBORw0KGgo=",
			want:  "iVBORw0KGgo=",
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "data URI missing base64 marker",
			input:   "data:image/png,rawdata",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeImageToBase64(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConvertContentParts_DataPart(t *testing.T) {
	msg := llm.Message{
		Role: llm.RoleUser,
		Parts: []llm.Part{
			{Type: llm.PartData, Data: &llm.DataRef{Value: map[string]any{"key": "value"}}},
		},
	}

	out := mustConvertMessages(t, []llm.Message{msg})
	if out[0].Content != "[ollama data]\nmime_type: application/json\njson:\n{\"key\":\"value\"}\n[/ollama data]" {
		t.Errorf("content = %q", out[0].Content)
	}
}

func TestConvertContentParts_DataPartBoundaries(t *testing.T) {
	msg := llm.Message{
		Role: llm.RoleUser,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "before"},
			{Type: llm.PartData, Data: &llm.DataRef{
				MimeType: "application/vnd.flowcraft+json",
				Value:    map[string]any{"first": "value"},
			}},
			{Type: llm.PartData, Data: &llm.DataRef{
				Value: map[string]any{"second": "value"},
			}},
			{Type: llm.PartText, Text: "after"},
		},
	}

	out := mustConvertMessages(t, []llm.Message{msg})
	want := "before\n\n" +
		"[ollama data]\n" +
		"mime_type: application/vnd.flowcraft+json\n" +
		"json:\n" +
		"{\"first\":\"value\"}\n" +
		"[/ollama data]\n\n" +
		"[ollama data]\n" +
		"mime_type: application/json\n" +
		"json:\n" +
		"{\"second\":\"value\"}\n" +
		"[/ollama data]\n\n" +
		"after"
	if out[0].Content != want {
		t.Fatalf("content = %q, want %q", out[0].Content, want)
	}
}

func TestConvertMessages_DataPartMarshalErrorIsValidation(t *testing.T) {
	_, err := convertMessages([]llm.Message{{
		Role: llm.RoleUser,
		Parts: []llm.Part{{
			Type: llm.PartData,
			Data: &llm.DataRef{Value: map[string]any{"bad": func() {}}},
		}},
	}})
	if !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want Validation", err)
	}
}

func TestConvertMessages_SystemPartDataValidation(t *testing.T) {
	_, err := convertMessages([]llm.Message{{
		Role: llm.RoleSystem,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "rules"},
			{Type: llm.PartData, Data: &llm.DataRef{Value: map[string]any{"k": "v"}}},
		},
	}})
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
	if !strings.Contains(err.Error(), "system message") {
		t.Fatalf("error should mention system message, got %q", err.Error())
	}
}

func TestGenerate_DataPartMarshalErrorPropagates(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer srv.Close()

	c, err := New("llama3", srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	bad := []llm.Message{{
		Role: llm.RoleUser,
		Parts: []llm.Part{{
			Type: llm.PartData,
			Data: &llm.DataRef{Value: map[string]any{"bad": func() {}}},
		}},
	}}

	if _, _, err := c.Generate(context.Background(), bad); !errdefs.IsValidation(err) {
		t.Fatalf("Generate err = %v, want Validation", err)
	}
	if _, err := c.GenerateStream(context.Background(), bad); !errdefs.IsValidation(err) {
		t.Fatalf("GenerateStream err = %v, want Validation", err)
	}
	if called {
		t.Fatal("server was called despite request conversion error")
	}
}

func TestConvertContentParts_FilePart(t *testing.T) {
	msg := llm.Message{
		Role: llm.RoleUser,
		Parts: []llm.Part{
			{Type: llm.PartFile, File: &llm.FileRef{URI: "gs://bucket/file.txt", MimeType: "text/plain"}},
		},
	}

	out := mustConvertMessages(t, []llm.Message{msg})
	if out[0].Content != "gs://bucket/file.txt" {
		t.Errorf("content = %q", out[0].Content)
	}
}

func TestConvertContentParts_ImageFilePart(t *testing.T) {
	msg := llm.Message{
		Role: llm.RoleUser,
		Parts: []llm.Part{
			{Type: llm.PartFile, File: &llm.FileRef{URI: "data:image/jpeg;base64,/9j/4AAQ", MimeType: "image/jpeg"}},
		},
	}

	out := mustConvertMessages(t, []llm.Message{msg})
	if len(out[0].Images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(out[0].Images))
	}
	if out[0].Images[0] != "/9j/4AAQ" {
		t.Errorf("image = %q", out[0].Images[0])
	}
}
