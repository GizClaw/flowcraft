package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdkx/inference/config"
	"github.com/GizClaw/flowcraft/sdkx/inference/openai"
)

func testSecret(t *testing.T, value string) config.Secret {
	t.Helper()
	secret, err := config.NewSecret([]byte(value))
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	return secret
}

func TestSpecValidation(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{
			name: "deployment",
			raw:  `{"endpoint":"https://res.openai.azure.com","models":[{"name":"chat-1","kind":"generate"}]}`,
			ok:   true,
		},
		{
			name: "api version",
			raw:  `{"endpoint":"https://res.openai.azure.com","api_version":"2025-03-01-preview","models":[{"name":"chat-1","kind":"generate","vision":true,"reasoning":true}]}`,
			ok:   true,
		},
		{name: "missing endpoint", raw: `{"models":[{"name":"m","kind":"generate"}]}`},
		{
			name: "non-https endpoint",
			raw:  `{"endpoint":"http://res.openai.azure.com","models":[{"name":"m","kind":"generate"}]}`,
		},
		{
			name: "no deployments",
			raw:  `{"endpoint":"https://res.openai.azure.com"}`,
		},
		{
			name: "duplicate deployment",
			raw:  `{"endpoint":"https://res.openai.azure.com","models":[{"name":"m","kind":"generate"},{"name":"m","kind":"embed"}]}`,
		},
		{
			name: "unknown kind",
			raw:  `{"endpoint":"https://res.openai.azure.com","models":[{"name":"m","kind":"video"}]}`,
		},
		{
			name: "vision on embed",
			raw:  `{"endpoint":"https://res.openai.azure.com","models":[{"name":"m","kind":"embed","vision":true}]}`,
		},
		{
			name: "dimensions on generate",
			raw:  `{"endpoint":"https://res.openai.azure.com","models":[{"name":"m","kind":"generate","dimensions":true}]}`,
		},
		{
			name: "bad version token",
			raw:  `{"endpoint":"https://res.openai.azure.com","api_version":"2025-01-01?x=1","models":[{"name":"m","kind":"generate"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeSpec([]byte(tc.raw))
			if tc.ok && err != nil {
				t.Fatalf("decodeSpec: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("decodeSpec succeeded, want validation error")
			}
		})
	}
}

func TestProfileMaterial(t *testing.T) {
	t.Run("missing api key", func(t *testing.T) {
		if _, err := newProfileMaterial(config.ResolvedProfile{ID: "default"}); err == nil {
			t.Fatal("newProfileMaterial succeeded without api_key")
		}
	})
	t.Run("unknown secret", func(t *testing.T) {
		_, err := newProfileMaterial(config.ResolvedProfile{
			ID:      "default",
			Secrets: map[string]config.Secret{"access_key": testSecret(t, "x")},
		})
		if err == nil {
			t.Fatal("newProfileMaterial accepted an unknown secret")
		}
	})
}

func TestFactoryBuild(t *testing.T) {
	input := config.ProviderInput{
		ID: "azure",
		Spec: json.RawMessage(`{
			"endpoint": "https://res.openai.azure.com",
			"models": [
				{"name": "chat-1", "kind": "generate", "vision": true},
				{"name": "embed-1", "kind": "embed", "dimensions": true},
				{"name": "stt-1", "kind": "asr"}
			]
		}`),
		Profiles: []config.ResolvedProfile{{
			ID:      "default",
			Secrets: map[string]config.Secret{SecretAPIKey: testSecret(t, "az-key")},
		}},
	}
	provider, err := Factory().Build(context.Background(), input)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if provider.ID != "azure" {
		t.Fatalf("provider ID = %q", provider.ID)
	}
	if len(provider.Models) != 3 {
		t.Fatalf("models = %d", len(provider.Models))
	}
	if provider.Models[0].Openers.Generate == nil {
		t.Fatal("generate deployment has no generate opener")
	}
	if provider.Models[1].Openers.Embed == nil {
		t.Fatal("embed deployment has no embed opener")
	}
	if provider.Models[2].Openers.Transcription == nil {
		t.Fatal("asr deployment has no transcription opener")
	}
}

// azureModel builds the model ref for one deployment.
func azureModel(name string) inference.ModelRef {
	return inference.ModelRef{
		ID:      inference.ModelID{Provider: "azure", Name: name},
		Profile: "default",
	}
}

// TestGenerateEndToEnd drives the kernel generate driver against a captured
// server and pins the Azure request shape: the deployment-scoped path plus
// the api-version query and api-key header.
func TestGenerateEndToEnd(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		if r.URL.Path != "/openai/deployments/chat-1/responses" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("api-version") != DefaultAPIVersion {
			t.Errorf("api-version = %q", r.URL.Query().Get("api-version"))
		}
		if r.Header.Get("api-key") != "az-key" {
			t.Errorf("api-key header = %q", r.Header.Get("api-key"))
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("bearer authorization must not be sent to azure")
		}
		payload, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(payload, &body)
		if model, _ := body["model"].(string); model != "chat-1" {
			t.Errorf("request model = %v", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id": "resp_1", "object": "response", "status": "completed",
			"output": [{"type": "message", "role": "assistant",
				"content": [{"type": "output_text", "text": "ok"}]}],
			"usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2,
				"input_tokens_details": {"cached_tokens": 0},
				"output_tokens_details": {"reasoning_tokens": 0}}
		}`)
	}))
	defer server.Close()

	spec, err := decodeSpec([]byte(fmt.Sprintf(
		`{"endpoint":%q,"models":[{"name":"chat-1","kind":"generate"}]}`,
		server.URL,
	)))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	cls := profileMaterial{apiKey: "az-key"}.newClients(spec)
	operations, err := openai.KernelGenerate(cls.api, "chat-1", openai.Capabilities{})
	if err != nil {
		t.Fatalf("KernelGenerate: %v", err)
	}
	response, err := operations.Unary.Execute(
		context.Background(),
		azureModel("chat-1"),
		inference.GenerateRequest{
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: "hi"},
					}},
					Intent: inference.Intent{Text: &inference.TextIntent{}},
				},
			},
		},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	text, ok := response.Message.Content.Parts[0].(message.TextPart)
	if !ok || text.Text != "ok" {
		t.Fatalf("part = %#v", response.Message.Content.Parts[0])
	}
}
