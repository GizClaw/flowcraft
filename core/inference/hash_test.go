package inference_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

func hashRequest(t *testing.T, toolNames ...string) inference.GenerateRequest {
	t.Helper()
	definitions := make([]message.ToolDefinition, 0, len(toolNames))
	for _, name := range toolNames {
		definitions = append(definitions, message.ToolDefinition{
			Name:        name,
			InputSchema: []byte(`{"type":"object"}`),
		})
	}
	tools, err := inference.NewToolset(definitions)
	if err != nil {
		t.Fatalf("NewToolset: %v", err)
	}
	return inference.GenerateRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: "p", Name: "m"}},
		Input: inference.GenerateInput{
			Role:    inference.InputRoleUser,
			Content: message.NewTextContent("hi"),
		},
		Context: []message.Message{
			message.NewTextMessage(message.RoleSystem, "sys"),
			message.NewTextMessage(message.RoleUser, "ctx"),
		},
		Tools: tools,
	}
}

func TestRequestHashIsStableAndSensitive(t *testing.T) {
	base := hashRequest(t, "read", "search")
	if got, want := inference.RequestHash(base), inference.RequestHash(hashRequest(t, "read", "search")); got != want {
		t.Fatalf("hash = %q, want %q", got, want)
	}
	// Tool declaration order is not part of the identity.
	if got, want := inference.RequestHash(hashRequest(t, "search", "read")), inference.RequestHash(base); got != want {
		t.Errorf("tool order changed the hash: %q != %q", got, want)
	}
	other := base
	other.Input.Content = message.NewTextContent("other")
	if inference.RequestHash(other) == inference.RequestHash(base) {
		t.Error("a changed input kept the hash")
	}
	other = base
	other.Model.ID.Name = "m2"
	if inference.RequestHash(other) == inference.RequestHash(base) {
		t.Error("a changed model kept the hash")
	}
	other = base
	other.Context = other.Context[:1]
	if inference.RequestHash(other) == inference.RequestHash(base) {
		t.Error("a changed context kept the hash")
	}
}
