package responses

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

func TestGenerate_UsesResponsesAPI(t *testing.T) {
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
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         "resp_1",
			"object":     "response",
			"created_at": float64(0),
			"model":      "gpt-test",
			"output": []map[string]any{{
				"type":   "message",
				"id":     "msg_1",
				"status": "completed",
				"role":   "assistant",
				"content": []map[string]any{{
					"type": "output_text",
					"text": "answer",
				}},
			}},
			"usage": map[string]any{
				"input_tokens":  3,
				"output_tokens": 4,
				"total_tokens":  7,
				"input_tokens_details": map[string]any{
					"cached_tokens": 1,
				},
			},
		})
	}))
	defer srv.Close()

	c, err := New("gpt-test", "test-key", srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	msg, usage, err := c.Generate(context.Background(), []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, "be concise"),
		{
			Role: llm.RoleUser,
			Parts: []llm.Part{
				{Type: llm.PartText, Text: "summarize"},
				{Type: llm.PartData, Data: &llm.DataRef{
					MimeType: "application/vnd.flowcraft.snapshot+json",
					Value:    map[string]any{"k": "v"},
				}},
			},
		},
	}, llm.WithMaxTokens(12))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if msg.Content() != "answer" {
		t.Fatalf("content = %q", msg.Content())
	}
	if usage.InputTokens != 3 || usage.OutputTokens != 4 || usage.TotalTokens != 7 || usage.CachedInputTokens != 1 {
		t.Fatalf("usage = %+v", usage)
	}
	if got["model"] != "gpt-test" || got["instructions"] != "be concise" || got["max_output_tokens"] != float64(12) {
		t.Fatalf("request body = %#v", got)
	}
	input := got["input"].([]any)
	user := input[0].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("input[0] = %#v", user)
	}
	content := user["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %#v", content)
	}
	if content[0].(map[string]any)["text"] != "summarize" {
		t.Fatalf("text content = %#v", content[0])
	}
	dataText := content[1].(map[string]any)["text"].(string)
	if !strings.Contains(dataText, "OpenAI input data") || !strings.Contains(dataText, "MIME type: application/vnd.flowcraft.snapshot+json") {
		t.Fatalf("data content = %q", dataText)
	}
}

func TestGenerate_RejectsNonCompletedResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want func(error) bool
	}{
		{
			name: "failed",
			body: `{"id":"resp_1","object":"response","created_at":0,"model":"gpt-test","status":"failed","error":{"code":"invalid_prompt","message":"bad prompt"},"output":[]}`,
			want: errdefs.IsValidation,
		},
		{
			name: "incomplete",
			body: `{"id":"resp_1","object":"response","created_at":0,"model":"gpt-test","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`,
			want: errdefs.IsValidation,
		},
		{
			name: "empty output",
			body: `{"id":"resp_1","object":"response","created_at":0,"model":"gpt-test","status":"completed","output":[]}`,
			want: errdefs.IsNotAvailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c, err := New("gpt-test", "test-key", srv.URL)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, _, err = c.Generate(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
			if !tc.want(err) {
				t.Fatalf("Generate error = %v", err)
			}
		})
	}
}

func TestBuildParams_SystemPartDataValidation(t *testing.T) {
	c, err := New("gpt-test", "test-key", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.buildParams([]llm.Message{{
		Role: llm.RoleSystem,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "rules"},
			{Type: llm.PartData, Data: &llm.DataRef{Value: map[string]any{"k": "v"}}},
		},
	}}, llm.ApplyOptions())
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestBuildParams_UserUnsupportedPartValidation(t *testing.T) {
	c, err := New("gpt-test", "test-key", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.buildParams([]llm.Message{{
		Role: llm.RoleUser,
		Parts: []llm.Part{{
			Type:  llm.PartAudio,
			Audio: &llm.MediaRef{Base64: "AA==", MediaType: "audio/mpeg"},
		}},
	}}, llm.ApplyOptions())
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
	for _, want := range []string{"openai responses", "user", string(llm.PartAudio)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("validation error missing %q: %v", want, err)
		}
	}
}

func TestBuildParams_FunctionToolsAreNotStrictByDefault(t *testing.T) {
	c, err := New("gpt-test", "test-key", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	params, err := c.buildParams([]llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")}, llm.ApplyOptions(
		llm.WithTools(llm.ToolDefinition{
			Name:        "lookup",
			Description: "look up a thing",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"q": map[string]any{"type": "string"},
				},
			},
		}),
	))
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if len(params.Tools) != 1 || params.Tools[0].OfFunction == nil {
		t.Fatalf("tools = %#v", params.Tools)
	}
	if strict := params.Tools[0].GetStrict(); strict == nil || *strict {
		t.Fatalf("tool strict = %v, want false", strict)
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
		_, _ = w.Write([]byte(`data: {"type":"response.completed","sequence_number":3,"response":{"id":"resp_1","object":"response","created_at":0,"model":"gpt-test","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3,"input_tokens_details":{"cached_tokens":0}}}}` + "\n\n"))
	}))
	defer srv.Close()

	c, err := New("gpt-test", "test-key", srv.URL)
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

func TestGenerateStream_IncompleteMaxTokensIsValidation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.incomplete\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.incomplete","sequence_number":1,"response":{"id":"resp_1","object":"response","created_at":0,"model":"gpt-test","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}}` + "\n\n"))
	}))
	defer srv.Close()

	c, err := New("gpt-test", "test-key", srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := c.GenerateStream(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	if stream.Next() {
		t.Fatal("Next returned true for incomplete event")
	}
	if err := stream.Err(); !errdefs.IsValidation(err) {
		t.Fatalf("Err = %v, want Validation", err)
	}
}

func TestGenerateStream_EventErrorUsesProviderName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: error\n"))
		_, _ = w.Write([]byte(`data: {"type":"error","code":"server_error","message":"boom","sequence_number":1}` + "\n\n"))
	}))
	defer srv.Close()

	c, err := New("gpt-test", "test-key", srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.WithProviderName("qwen")
	stream, err := c.GenerateStream(context.Background(), []llm.Message{llm.NewTextMessage(llm.RoleUser, "hi")})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	if stream.Next() {
		t.Fatal("Next returned true for error event")
	}
	err = stream.Err()
	if !errdefs.IsNotAvailable(err) || !strings.Contains(err.Error(), "qwen responses") {
		t.Fatalf("Err = %v, want qwen NotAvailable", err)
	}
}
