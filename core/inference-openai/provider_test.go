package openai

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
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

// Decoding a schema per request is a full json.Unmarshal per tool; the
// definition never changes after construction.
func TestSchemaObjectIsCached(t *testing.T) {
	definition := message.ToolDefinition{
		Name:        "read_file",
		InputSchema: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	}
	if _, err := schemaObject(definition); err != nil {
		t.Fatalf("schemaObject: %v", err)
	}
	calls := func() { _, _ = schemaObject(definition) }
	if got := testing.AllocsPerRun(100, calls); got > 3 {
		t.Fatalf("schemaObject allocates %.0f times after warm-up, want at most 3 "+
			"(key plus slack); a decode would be many more", got)
	}
}

func BenchmarkSchemaObject(b *testing.B) {
	definition := message.ToolDefinition{
		Name:        "read_file",
		InputSchema: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := schemaObject(definition); err != nil {
			b.Fatal(err)
		}
	}
}
