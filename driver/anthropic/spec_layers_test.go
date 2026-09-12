package anthropic

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// TestCatalogDeclaredMode pins the namespace switch: a compatible Messages
// endpoint (MiniMax's /anthropic surface and the like) must not inherit
// Claude's built-in facts just because a model name collides.
func TestCatalogDeclaredMode(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"catalog": "declared",
		"endpoint": {"base_url": "https://api.minimaxi.com/anthropic"},
		"models": [{
			"name": "MiniMax-M3",
			"capabilities": {"inputs": ["text"], "outputs": ["text"]}
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("declared catalog built %d models, want 1", len(models))
	}
	entry, ok := models["MiniMax-M3"]
	if !ok {
		t.Fatalf("models = %v", models)
	}
	if len(entry.capabilities.Inputs) != 1 ||
		entry.capabilities.Inputs[0] != message.PartText {
		t.Fatalf("built-in facts leaked: %+v", entry.capabilities.Inputs)
	}

	inherited, err := decodeSpec(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	builtins, err := mergedCatalog(inherited)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	if len(builtins) != len(catalog) {
		t.Fatalf("builtin_declared kept %d models, want %d", len(builtins), len(catalog))
	}
}

// TestSpecValidationLayers covers the added configuration surface: the
// endpoint move, the catalog enum, and the kind guard that keeps media
// models off the Messages protocol.
func TestSpecValidationLayers(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{name: "empty", raw: `{}`, ok: true},
		{name: "endpoint base url",
			raw: `{"endpoint":{"base_url":"https://api.minimaxi.com/anthropic"}}`, ok: true},
		{name: "endpoint base url malformed",
			raw: `{"endpoint":{"base_url":"api.minimaxi.com"}}`, ok: false},
		{name: "legacy flat base url is gone",
			raw: `{"base_url":"https://gateway.example.com"}`, ok: false},
		{name: "declared catalog", raw: `{"catalog":"declared"}`, ok: true},
		{name: "unknown catalog", raw: `{"catalog":"builtin"}`, ok: false},
		{name: "generate kind",
			raw: `{"models":[{"name":"m","kind":"generate"}]}`, ok: true},
		{name: "media kind is rejected",
			raw: `{"models":[{"name":"m","kind":"video"}]}`, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeSpec(context.Background(), []byte(tc.raw))
			if tc.ok && err != nil {
				t.Fatalf("decodeSpec(%s): %v", tc.raw, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("decodeSpec(%s): want error", tc.raw)
			}
		})
	}
}

// TestDeclaredCatalogNeedsTextOutput keeps the family contract: a declared
// Messages model must publish text output, exactly like a built-in one.
func TestDeclaredCatalogNeedsTextOutput(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"catalog": "declared",
		"models": [{"name": "m", "capabilities": {"outputs": ["image"]}}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	_, err = mergedCatalog(spec)
	if err == nil || !strings.Contains(err.Error(), "text output") {
		t.Fatalf("mergedCatalog error = %v, want a text-output rejection", err)
	}
}
