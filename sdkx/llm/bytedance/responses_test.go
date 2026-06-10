package bytedance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"

	arkresponses "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model/responses"
)

func TestGenerate_UsesResponsesWithWebSearchConfig(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %q, want /responses", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Fatalf("Authorization = %q", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output": []map[string]any{{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{{
					"type": "output_text",
					"text": "answer with search",
				}},
			}},
			"usage": map[string]any{
				"input_tokens":  3,
				"output_tokens": 4,
				"total_tokens":  7,
			},
		})
	}))
	defer srv.Close()

	c, err := New("doubao-seed-2-0-lite-260215", "test-key", srv.URL, "", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.webSearch = webSearchConfig{Enabled: true, MaxKeyword: 2, Limit: 10}

	msg, usage, err := c.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, "be concise"),
		llm.NewTextMessage(llm.RoleUser, "what changed today?"),
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if msg.Content() != "answer with search" {
		t.Fatalf("content = %q", msg.Content())
	}
	if usage.InputTokens != 3 || usage.OutputTokens != 4 || usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v", usage)
	}
	if got["model"] != "doubao-seed-2-0-lite-260215" {
		t.Fatalf("model = %v", got["model"])
	}
	if got["instructions"] != "be concise" {
		t.Fatalf("instructions = %v", got["instructions"])
	}
	input, ok := got["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input = %#v", got["input"])
	}
	msg0, _ := input[0].(map[string]any)
	if msg0["role"] != "user" || msg0["content"] != "what changed today?" {
		t.Fatalf("input[0] = %#v", msg0)
	}
	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v", got["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "web_search" || tool["max_keyword"] != float64(2) || tool["limit"] != float64(10) {
		t.Fatalf("tool = %#v", tool)
	}
}

func TestGenerate_WithExtraOverridesWebSearch(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output": []map[string]any{{
				"type":    "message",
				"role":    "assistant",
				"content": []map[string]any{{"type": "output_text", "text": "ok"}},
			}},
		})
	}))
	defer srv.Close()

	c, err := New("doubao-test", "test-key", srv.URL, "", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, _, err = c.Generate(
		context.Background(),
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")},
		llm.WithExtra("web_search", map[string]any{
			"enabled":     true,
			"max_keyword": float64(4),
			"limit":       float64(20),
		}),
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	tools := got["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["type"] != "web_search" || tool["max_keyword"] != float64(4) || tool["limit"] != float64(20) {
		t.Fatalf("tool = %#v", tool)
	}
}

func TestGenerate_UsesResponsesImageContent(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output": []map[string]any{{
				"type":    "message",
				"role":    "assistant",
				"content": []map[string]any{{"type": "output_text", "text": "seen"}},
			}},
		})
	}))
	defer srv.Close()

	c, err := New("doubao-test", "test-key", srv.URL, "", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = c.Generate(context.Background(), []llm.Message{{
		Role: llm.RoleUser,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "describe"},
			{Type: llm.PartImage, Image: &llm.MediaRef{URL: "https://example.test/image.png"}},
		},
	}})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	input := got["input"].([]any)
	msg := input[0].(map[string]any)
	content := msg["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %#v", content)
	}
	text := content[0].(map[string]any)
	if text["type"] != "input_text" || text["text"] != "describe" {
		t.Fatalf("text content = %#v", text)
	}
	image := content[1].(map[string]any)
	if image["type"] != "input_image" || image["image_url"] != "https://example.test/image.png" {
		t.Fatalf("image content = %#v", image)
	}
}

func TestGenerate_UsesResponsesFileContent(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output": []map[string]any{{
				"type":    "message",
				"role":    "assistant",
				"content": []map[string]any{{"type": "output_text", "text": "read"}},
			}},
		})
	}))
	defer srv.Close()

	c, err := New("doubao-test", "test-key", srv.URL, "", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = c.Generate(context.Background(), []llm.Message{{
		Role: llm.RoleUser,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "summarize"},
			{Type: llm.PartFile, File: &llm.FileRef{URI: "file_id://file_123", MimeType: "application/pdf", Name: "doc.pdf"}},
			{Type: llm.PartFile, File: &llm.FileRef{URI: "https://example.test/report.csv", MimeType: "text/csv"}},
		},
	}})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	input := got["input"].([]any)
	msg := input[0].(map[string]any)
	content := msg["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content = %#v", content)
	}
	if text := content[0].(map[string]any); text["type"] != "input_text" || text["text"] != "summarize" {
		t.Fatalf("text content = %#v", text)
	}
	fileID := content[1].(map[string]any)
	if fileID["type"] != "input_file" || fileID["file_id"] != "file_123" || fileID["filename"] != "doc.pdf" {
		t.Fatalf("file_id content = %#v", fileID)
	}
	fileURL := content[2].(map[string]any)
	if fileURL["type"] != "input_file" || fileURL["file_url"] != "https://example.test/report.csv" {
		t.Fatalf("file_url content = %#v", fileURL)
	}
}

func TestGenerateStream_ResponsesChunks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if got["stream"] != true {
			t.Fatalf("stream = %#v", got["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.output_text.delta\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"hel","content_index":0,"item_id":"msg","output_index":0,"sequence_number":1}` + "\n\n"))
		_, _ = w.Write([]byte("event: response.output_text.delta\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"lo","content_index":0,"item_id":"msg","output_index":0,"sequence_number":2}` + "\n\n"))
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.completed","sequence_number":3,"response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}` + "\n\n"))
	}))
	defer srv.Close()

	c, err := New("doubao-test", "test-key", srv.URL, "", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := c.GenerateStream(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	var chunks []string
	for stream.Next() {
		chunks = append(chunks, stream.Current().Content)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if strings.Join(chunks, "") != "hello" || len(chunks) != 2 {
		t.Fatalf("chunks = %#v", chunks)
	}
	if got := stream.Message().Content(); got != "hello" {
		t.Fatalf("Message = %q", got)
	}
	if got := stream.Usage(); got.InputTokens != 1 || got.OutputTokens != 2 {
		t.Fatalf("Usage = %+v", got)
	}
}

func TestResponsesStreamBuffersSplitUTF8Delta(t *testing.T) {
	var stream responsesStreamMessage
	first := stream.appendDeltaTextLocked(string([]byte{0xe4, 0xbd}))
	if first != "" {
		t.Fatalf("first split chunk = %q, want buffered", first)
	}
	second := stream.appendDeltaTextLocked(string([]byte{0xa0}) + "好")
	if second != "你好" {
		t.Fatalf("second chunk = %q, want 你好", second)
	}
	if !utf8.ValidString(second) {
		t.Fatalf("second chunk is not valid utf8: %q", second)
	}
	if stream.content != "你好" || len(stream.pending) != 0 {
		t.Fatalf("content=%q pending=%x, want complete output", stream.content, stream.pending)
	}
}

func TestGenerateStream_AccumulatesToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if got["stream"] != true {
			t.Fatalf("stream = %#v", got["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.output_item.added\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.output_item.added","output_index":0,"sequence_number":1,"item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":""}}` + "\n\n"))
		_, _ = w.Write([]byte("event: response.function_call_arguments.delta\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.function_call_arguments.delta","output_index":0,"sequence_number":2,"item_id":"fc_1","delta":"{\"q\""}` + "\n\n"))
		_, _ = w.Write([]byte("event: response.function_call_arguments.delta\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.function_call_arguments.delta","output_index":0,"sequence_number":3,"item_id":"fc_1","delta":":\"news\"}"}` + "\n\n"))
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.completed","sequence_number":4,"response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"calling lookup"}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}` + "\n\n"))
	}))
	defer srv.Close()

	c, err := New("doubao-test", "test-key", srv.URL, "", 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := c.GenerateStream(
		context.Background(),
		[]llm.Message{llm.NewTextMessage(llm.RoleUser, "search")},
		llm.WithTools(llm.ToolDefinition{
			Name:        "lookup",
			Description: "look up current facts",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"q": map[string]any{"type": "string"}},
			},
		}),
	)
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	for stream.Next() {
		t.Fatalf("unexpected text chunk: %#v", stream.Current())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	msg := stream.Message()
	if msg.Content() != "calling lookup" {
		t.Fatalf("Content = %q", msg.Content())
	}
	calls := msg.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1; message=%#v", len(calls), msg)
	}
	if calls[0].ID != "call_1" || calls[0].Name != "lookup" || calls[0].Arguments != `{"q":"news"}` {
		t.Fatalf("ToolCall = %#v", calls[0])
	}
	if got := stream.Usage(); got.InputTokens != 2 || got.OutputTokens != 1 {
		t.Fatalf("Usage = %+v", got)
	}
}

func TestResponsesStream_ClassifiesEventErrors(t *testing.T) {
	code := "RateLimitExceeded"
	streamErr := (&responsesStreamMessage{}).applyEvent(&arkresponses.Event{Event: &arkresponses.Event_Error{
		Error: &arkresponses.ErrorEvent{Code: &code, Message: "too many requests"},
	}})
	if !errdefs.IsRateLimit(streamErr) {
		t.Fatalf("stream event error = %v, want RateLimit", streamErr)
	}

	failedErr := (&responsesStreamMessage{}).applyEvent(&arkresponses.Event{Event: &arkresponses.Event_ResponseFailed{
		ResponseFailed: &arkresponses.ResponseFailedEvent{Response: &arkresponses.ResponseObject{
			Error: &arkresponses.Error{Code: "InvalidParameter", Message: "bad request"},
		}},
	}})
	if !errdefs.IsValidation(failedErr) {
		t.Fatalf("response failed error = %v, want Validation", failedErr)
	}
}

func TestGenerate_HTTPErrorClassified(t *testing.T) {
	tests := []struct {
		name   string
		status int
		check  func(error) bool
	}{
		{name: "401", status: http.StatusUnauthorized, check: errdefs.IsUnauthorized},
		{name: "429", status: http.StatusTooManyRequests, check: errdefs.IsRateLimit},
		{name: "500", status: http.StatusInternalServerError, check: errdefs.IsNotAvailable},
		{name: "400", status: http.StatusBadRequest, check: errdefs.IsValidation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			}))
			defer srv.Close()

			c, err := New("doubao-test", "test-key", srv.URL, "", 0)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, _, err = c.Generate(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
			if err == nil {
				t.Fatal("expected error")
			}
			if !tc.check(err) {
				t.Fatalf("wrong classification: %v", err)
			}
		})
	}
}
