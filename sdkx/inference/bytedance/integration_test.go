package bytedance_test

// End-to-end proof of the deployment path: a versioned config Document
// (provider envelope + secret references + route policy) flows through
// config.Builder with this package's Factory registered under the driver
// name, and every provider-owned config item lands at its destination —
// base_url redirects transport, endpoints rewrite the addressed model,
// profile secrets authenticate, and the route section composes a Router.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdkx/inference/bytedance"
	"github.com/GizClaw/flowcraft/sdkx/inference/config"
	"github.com/GizClaw/flowcraft/sdkx/inference/config/env"
)

// TestConfigToBytedanceInstance assembles a Runtime+Router from a Document
// and runs a Generate through it, asserting each config item's effect.
func TestConfigToBytedanceInstance(t *testing.T) {
	var captured map[string]any
	server := newArkServer(t, func(body map[string]any) {
		captured = body
	})
	defer server.Close()

	t.Setenv("ARK_API_KEY", "deployment-key")

	// The document as a deployment would write it: driver selects the
	// factory, spec carries provider-owned settings, secrets stay references.
	documentJSON := fmt.Sprintf(`{
		"version": "v1",
		"providers": [{
			"id": "bytedance",
			"driver": "bytedance",
			"profiles": [{
				"id": "default",
				"secrets": {"api_key": {"resolver": "env", "key": "ARK_API_KEY"}}
			}],
			"spec": {
				"base_url": "%s/api/v3",
				"endpoints": {"doubao-seed-2-1-pro": "ep-deploy-123"},
				"models": [{"name": "my-alias", "kind": "generate", "vision": true}]
			}
		}],
		"route": {
			"generate": [{
				"tier": "primary",
				"targets": [{"model": {
					"id": {"provider": "bytedance", "name": "doubao-seed-2-1-pro"},
					"profile": "default"
				}}]
			}]
		}
	}`, server.URL)
	document, err := config.DecodeJSON(strings.NewReader(documentJSON))
	if err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}

	// Registration is explicit: driver name → factory, resolver name →
	// resolver. Nothing is process-global.
	builder, err := config.NewBuilder(
		map[string]config.Factory{"bytedance": bytedance.Factory()},
		map[string]config.SecretResolver{"env": env.New()},
	)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	assembly, err := builder.NewAssembly(context.Background(), document)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	if assembly.Router == nil {
		t.Fatal("route section should compose a Router")
	}

	// The custom model declaration is part of the runtime catalog.
	descriptor, err := assembly.Runtime.InspectModel(inference.ModelRef{
		ID:      inference.ModelID{Provider: "bytedance", Name: "my-alias"},
		Profile: "default",
	})
	if err != nil {
		t.Fatalf("InspectModel: %v", err)
	}
	if descriptor.ID.Name != "my-alias" {
		t.Fatalf("descriptor = %+v", descriptor)
	}

	// Drive a request through the Router: policy selection, target
	// validation, and execution happen in one call.
	response, trace, err := assembly.Router.Generate(
		context.Background(),
		inference.GenerateRequest{
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: inference.Content{
						Parts: []inference.Part{inference.TextPart{Text: "hi"}},
					},
					Intent: inference.Intent{Text: &inference.TextIntent{}},
				},
			},
		},
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(response.Message.Content.Parts) == 0 {
		t.Fatal("empty response")
	}
	if trace.Executed.ID.Name != "doubao-seed-2-1-pro" ||
		trace.Executed.Profile != "default" {
		t.Fatalf("executed = %+v", trace.Executed)
	}
	// base_url redirected transport here, and endpoints rewrote the model.
	if captured["model"] != "ep-deploy-123" {
		t.Fatalf("model = %v, want ep-deploy-123", captured["model"])
	}
}

// newArkServer serves one canned Responses API reply and records the body.
func newArkServer(
	t *testing.T,
	record func(body map[string]any),
) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		if record != nil {
			record(body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id": "resp_1", "object": "response", "status": "completed",
			"output": [{
				"type": "message", "role": "assistant",
				"content": [{"type": "output_text", "text": "ok"}]
			}],
			"usage": {"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}
		}`)
	}))
	return server
}
