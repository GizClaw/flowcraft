package anthropic

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

// TestRetiredCatalogKey covers the namespace switch that no longer exists: a
// compatible Messages endpoint (MiniMax's /anthropic surface and the like)
// serves exactly the models its deployment declares, and a document that
// still selects a namespace fails with the migration path.
func TestRetiredCatalogKey(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"endpoint": {"base_url": "https://api.minimaxi.com/anthropic"},
		"models": [{"name": "MiniMax-M3",
			"capabilities": {"inputs": ["text"], "outputs": ["text"]}}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := resolveModelsForTest(t, spec)
	if err != nil {
		t.Fatalf("resolveModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("declaration built %d models, want 1", len(models))
	}
	entry, ok := models["MiniMax-M3"]
	if !ok {
		t.Fatalf("models = %v", models)
	}
	if len(entry.spec.Capabilities.Inputs) != 1 ||
		entry.spec.Capabilities.Inputs[0] != message.PartText {
		t.Fatalf("undeclared facts leaked: %+v", entry.spec.Capabilities.Inputs)
	}

	// No declarations, no models: the driver ships no line-up to fall back on.
	empty := decodeSpecWithModels(t, "")
	if built := mustResolve(t, empty); len(built) != 0 {
		t.Fatalf("an empty spec built %d models, want none", len(built))
	}

	for _, raw := range []string{
		`{"catalog":"declared"}`,
		`{"catalog":"builtin_declared"}`,
		`{"catalog":"builtin"}`,
	} {
		_, err := decodeSpec(context.Background(), []byte(raw))
		if err == nil {
			t.Fatalf("decodeSpec(%s) accepted a retired catalog key", raw)
		}
		if !strings.Contains(err.Error(), "declare") {
			t.Fatalf("error = %v, want the declaration migration path", err)
		}
	}
}

// TestSpecValidationLayers covers the configuration surface: the endpoint
// move, the retired catalog key, and the kind guard that keeps media models
// off the Messages protocol.
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
		{name: "retired catalog key",
			raw: `{"catalog":"declared"}`, ok: false},
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

// TestDeclaredModelNeedsTextOutput keeps the family contract: a declared
// Messages model must publish text output.
func TestDeclaredModelNeedsTextOutput(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(`{
		"models": [{"name": "m", "capabilities": {"outputs": ["image"]}}]
	}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	_, err = resolveModelsForTest(t, spec)
	if err == nil || !strings.Contains(err.Error(), "text output") {
		t.Fatalf("resolveModels error = %v, want a text-output rejection", err)
	}
}

// mustResolve resolves a spec in tests that only care about the count.
func mustResolve(t *testing.T, spec Spec) map[string]testTarget {
	t.Helper()
	models, err := resolveModelsForTest(t, spec)
	if err != nil {
		t.Fatalf("resolveModels: %v", err)
	}
	return models
}
