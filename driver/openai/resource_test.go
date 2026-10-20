package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestResourceFactoryBuildsProviderWithEnvSecret(t *testing.T) {
	t.Setenv("OPENAI_TEST_KEY", "sk-test")
	settings, err := resource.Expand(context.Background(),
		json.RawMessage(`{
			"id": "openai",
			"spec": {"endpoint": {"organization": "org-1"}, "models": `+
			fixtureModelsJSON(t, fixtureNames...)+`},
			"profiles": [{
				"id": "default",
				"operations": ["generate", "embed"],
				"secrets": {"api_key": "${env:OPENAI_TEST_KEY}"}
			}]
		}`), resource.ExpandEnv())
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	value, err := Factory().New(context.Background(), resource.NewInput(settings, nil, nil, nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider := value
	if provider.ID != "openai" || len(provider.Profiles) != 1 ||
		len(provider.Models) != len(fixtureNames) {
		t.Fatalf("provider = %+v", provider)
	}
}

// TestResourceDecisionOpenerServesTheWire drives the decision opener of a
// provider built the way a deployment builds one: a settings document, a
// profile carrying the credential, and declared models. Every other offline
// decision test binds the pipeline from fixtures, so this is the one that
// covers what only the assembled provider carries — the profile the opener
// resolves, the key it attaches, and the declaration the compiler narrows to.
func TestResourceDecisionOpenerServesTheWire(t *testing.T) {
	t.Setenv("OPENAI_TEST_KEY", "sk-test")
	var auth string
	server, capture := newCapturedOpenAI(t, func(
		w http.ResponseWriter,
		r *http.Request,
		_ map[string]any,
	) {
		assertDecisionRoute(t, r)
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, decisionResponseJSON())
	})
	defer server.Close()

	settings, err := resource.Expand(context.Background(),
		json.RawMessage(`{
			"id": "openai",
			"spec": {"endpoint": {"base_url": "`+server.URL+`"}, "models": `+
			fixtureModelsJSON(t, "gpt-6-luna")+`},
			"profiles": [{
				"id": "default",
				"operations": ["decision"],
				"secrets": {"api_key": "${env:OPENAI_TEST_KEY}"}
			}]
		}`), resource.ExpandEnv())
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	value, err := Factory().New(context.Background(), resource.NewInput(settings, nil, nil, nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider := value
	var declared *inference.ModelImplementation
	for index := range provider.Models {
		if provider.Models[index].Descriptor.ID.Name == "gpt-6-luna" {
			declared = &provider.Models[index]
		}
	}
	if declared == nil {
		t.Fatal("gpt-6-luna missing from the provider's models")
	}

	driver, err := declared.Openers.Decision(
		context.Background(), openaiModel("gpt-6-luna"))
	if err != nil {
		t.Fatalf("decision opener: %v", err)
	}
	response, err := driver.Execute(
		context.Background(), openaiModel("gpt-6-luna"), decisionRequestFixture())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if response.Model != "gpt-6-luna-2026-09-01" || len(response.Answers) != 3 {
		t.Fatalf("response = %+v", response)
	}
	body := capture.body(0)
	if body["model"] != "gpt-6-luna" || body["input"] != "the evidence" {
		t.Fatalf("body = %+v", body)
	}
	// The opener resolved the profile's secret reference, so the key the
	// settings document declared is what authenticated the call.
	if auth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q, want the profile's key", auth)
	}
}

func TestResourceSettingsHTTPRetriesFromEnv(t *testing.T) {
	t.Setenv("OPENAI_TEST_HTTP_RETRIES", "3")
	settings, err := resource.DecodeTyped[ResourceSettings](
		context.Background(),
		json.RawMessage(`{
			"id": "openai",
			"spec": {"http_retries": "${env:OPENAI_TEST_HTTP_RETRIES}"}
		}`),
		resource.ExpandEnv())
	if err != nil {
		t.Fatalf("DecodeTyped: %v", err)
	}
	spec, err := decodeSpec(context.Background(), settings.Spec)
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	if spec.HTTPRetries == nil || int(*spec.HTTPRetries) != 3 {
		t.Fatalf("HTTPRetries = %v, want 3", spec.HTTPRetries)
	}
}

func TestResourceFactoryRequiresID(t *testing.T) {
	if _, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{"profiles":[]}`),
	}); err == nil {
		t.Fatal("New accepted settings without id")
	}
	// A profile without api_key builds fine: missing keys surface at
	// request time (lazy secret resolution), not at deploy time.
	if _, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "openai",
			"profiles": [{"id": "default"}]
		}`),
	}); err != nil {
		t.Fatalf("New rejected a profile without api_key: %v", err)
	}
}

func TestRegisterAddsOpenAIProviderFactory(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Lookup(ResourceKind, "openai"); !ok {
		t.Fatalf("factory %s/openai missing", ResourceKind)
	}
}

func TestProviderCarriesGenerateOptionsDecoder(t *testing.T) {
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "openai",
			"profiles": [{"id": "default", "secrets": {"api_key": "sk-test"}}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider := value
	decoder, ok := provider.ExtensionDecoders[extensionGenerate]
	if !ok {
		t.Fatalf("ExtensionDecoders = %#v, want %q", provider.ExtensionDecoders, extensionGenerate)
	}

	extensions, err := inference.DecodeExtensions([]inference.ExtensionEntry{{
		Provider: "openai",
		ID:       extensionGenerate,
		Fields: json.RawMessage(`{
			"web_search": {
				"tool_choice": {"required": true},
				"search_context_size": "high"
			}
		}`),
	}}, map[string]inference.ExtensionDecoder{
		"openai/" + extensionGenerate: decoder,
	}, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	options := extensions[0].(*GenerateOptions)
	if options.ProviderID() != "openai" || options.WebSearch == nil ||
		options.WebSearch.ToolChoice == nil || !options.WebSearch.ToolChoice.Required ||
		options.WebSearch.SearchContextSize != "high" {
		t.Fatalf("decoded options = %#v", options)
	}

	// A renamed deployment still decodes: the decoder is bound to the
	// deployment provider ID, not the driver default.
	value, err = Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "oa-prod",
			"profiles": [{"id": "default", "secrets": {"api_key": "sk-test"}}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	decoder = value.ExtensionDecoders[extensionGenerate]
	extensions, err = inference.DecodeExtensions([]inference.ExtensionEntry{{
		Provider: "oa-prod",
		ID:       extensionGenerate,
	}}, map[string]inference.ExtensionDecoder{
		"oa-prod/" + extensionGenerate: decoder,
	}, "extensions")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	if options := extensions[0].(*GenerateOptions); options.ProviderID() != "oa-prod" {
		t.Fatalf("ProviderID = %q, want %q", options.ProviderID(), "oa-prod")
	}
}
