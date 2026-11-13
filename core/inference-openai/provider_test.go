package openai

import (
	"context"
	"os"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/internal/cache"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

func testSettings(api API) Settings {
	return Settings{API: api, APIKey: "test-key", BaseURL: "http://localhost"}
}

func canonicalRequest(t *testing.T) inference.GenerateRequest {
	t.Helper()
	tools, err := inference.NewToolset([]message.ToolDefinition{{
		Name:        "search",
		Description: "search the corpus",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
	}})
	if err != nil {
		t.Fatalf("toolset: %v", err)
	}
	tools, err = tools.Choosing(inference.ToolChoice{Mode: inference.ToolChoiceAuto})
	if err != nil {
		t.Fatalf("choosing: %v", err)
	}
	request := inference.GenerateRequest{
		Model: model.ModelRef{
			ID:      model.ModelID{Provider: ProviderID, Name: "gpt-5-mini"},
			Profile: "default",
		},
		Context: []message.Message{
			message.NewTextMessage(message.RoleSystem, "be brief"),
			message.NewTextMessage(message.RoleUser, "earlier"),
		},
		Input: inference.GenerateInput{
			Role:    inference.InputRoleUser,
			Content: message.NewTextContent("hello"),
		},
		Tools: tools,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("request: %v", err)
	}
	return request
}

// wireMap marshals a compiled wire value and decodes it into a generic map, which
// is the conformance assertion: the mapping is checked against the bytes that
// would go over the network, with no network involved.
func wireMap(t *testing.T, wire any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// TestCompileRejectsUnsupportedOutput proves the driver refuses what it cannot
// map instead of silently dropping it.
// TestLiveChat is the credentialed lane: it runs only when an API key is present
// and skips otherwise, so `go test ./...` stays hermetic.
func TestLiveChat(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	for _, api := range []API{APIChat, APIResponses} {
		t.Run(string(api), func(t *testing.T) {
			provider, err := New(Settings{API: api, APIKey: key})
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			request := inference.GenerateRequest{
				Model: model.ModelRef{
					ID:      model.ModelID{Provider: ProviderID, Name: "gpt-4o-mini"},
					Profile: "default",
				},
				Input: inference.GenerateInput{
					Role:    inference.InputRoleUser,
					Content: message.NewTextContent("Reply with exactly: pong"),
				},
			}
			response, err := provider.Generate(context.Background(), request)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			t.Logf("%s -> %q (finish=%s, in=%d out=%d)",
				api, response.Message.Content.Text(), response.FinishReason,
				response.Usage.InputTokens, response.Usage.OutputTokens)
			if response.Message.Content.Text() == "" {
				t.Fatal("empty answer")
			}
		})
	}
}

// Decoding a schema per request is a full json.Unmarshal per tool; the definition
// never changes after construction, so one instance keeps the decoded copy.
func TestSchemaObjectIsCached(t *testing.T) {
	definition := message.ToolDefinition{
		Name:        "read_file",
		InputSchema: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	}
	d := &driver{schemas: cache.New[map[string]any](8)}
	first, err := d.schemaObject(definition)
	if err != nil {
		t.Fatalf("schemaObject: %v", err)
	}
	calls := func() { _, _ = d.schemaObject(definition) }
	if got := testing.AllocsPerRun(100, calls); got > 3 {
		t.Fatalf("schemaObject allocates %.0f times after warm-up, want at most 3 "+
			"(key plus slack); a decode would be many more", got)
	}
	// A hit must return the same decoded object, not a re-decoded copy.
	second, err := d.schemaObject(definition)
	if err != nil {
		t.Fatalf("schemaObject: %v", err)
	}
	if len(second) != len(first) {
		t.Fatalf("cached object has %d keys, first had %d", len(second), len(first))
	}
	if got := d.schemas.Len(); got != 1 {
		t.Fatalf("cache holds %d entries, want 1", got)
	}
}

func BenchmarkSchemaObject(b *testing.B) {
	definition := message.ToolDefinition{
		Name:        "read_file",
		InputSchema: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	}
	d := &driver{schemas: cache.New[map[string]any](8)}
	b.ReportAllocs()
	for range b.N {
		if _, err := d.schemaObject(definition); err != nil {
			b.Fatal(err)
		}
	}
}

// TestResponseFormatSharesSchemaCache pins the sharing: a response-format schema is
// decoded by the same cache as a tool schema, because the value is the same kind of
// object, so one entry answers both.
func TestResponseFormatSharesSchemaCache(t *testing.T) {
	schema := []byte(`{"type":"object","properties":{"answer":{"type":"string"}}}`)
	format := ResponseFormat{Kind: FormatJSONSchema, Name: "out", Schema: schema}
	d := &driver{schemas: cache.New[map[string]any](8)}

	if _, err := d.chatResponseFormat(format); err != nil {
		t.Fatalf("chatResponseFormat: %v", err)
	}
	if got := d.schemas.Len(); got != 1 {
		t.Fatalf("cache holds %d entries after one format, want 1", got)
	}
	if _, err := d.chatResponseFormat(format); err != nil {
		t.Fatalf("chatResponseFormat again: %v", err)
	}
	if got := d.schemas.Len(); got != 1 {
		t.Fatalf("cache holds %d entries after a repeat, want 1", got)
	}

	// A tool that declares the identical schema must hit the same entry.
	if _, err := d.schemaObject(message.ToolDefinition{Name: "emit", InputSchema: schema}); err != nil {
		t.Fatalf("schemaObject: %v", err)
	}
	if got := d.schemas.Len(); got != 1 {
		t.Fatalf("cache holds %d entries after a shared tool schema, want 1", got)
	}

	// The Responses surface reads the same cache through its own union.
	if _, err := d.responsesResponseFormat(format); err != nil {
		t.Fatalf("responsesResponseFormat: %v", err)
	}
	if got := d.schemas.Len(); got != 1 {
		t.Fatalf("cache holds %d entries after the Responses path, want 1", got)
	}
}
