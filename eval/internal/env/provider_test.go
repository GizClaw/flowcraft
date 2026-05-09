package env

import (
	"strings"
	"testing"
)

func TestLoad_FlowcraftJSON(t *testing.T) {
	t.Setenv("FLOWCRAFT_AZURE", `{
		"api_key":"sk-test",
		"model":"gpt-5.4",
		"base_url":"https://example.azure.com",
		"api_version":"2024-08-01-preview",
		"caps":{"no_temperature":true}
	}`)

	provider, model, cfg, err := Load("azure")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if provider != "azure" {
		t.Errorf("provider = %q, want azure", provider)
	}
	if model != "gpt-5.4" {
		t.Errorf("model = %q, want gpt-5.4", model)
	}
	if cfg["api_key"] != "sk-test" {
		t.Errorf("api_key = %v, want sk-test", cfg["api_key"])
	}
	if cfg["base_url"] != "https://example.azure.com" {
		t.Errorf("base_url = %v", cfg["base_url"])
	}
	if cfg["api_version"] != "2024-08-01-preview" {
		t.Errorf("api_version = %v", cfg["api_version"])
	}
	if _, present := cfg["model"]; present {
		t.Errorf("cfg should not carry model after Load: %v", cfg)
	}
	if _, present := cfg["provider"]; present {
		t.Errorf("cfg should not carry provider after Load: %v", cfg)
	}
	caps, ok := cfg["caps"].(map[string]any)
	if !ok || caps["no_temperature"] != true {
		t.Errorf("caps not preserved: %v", cfg["caps"])
	}
}

func TestLoad_FallbackToTestEnv(t *testing.T) {
	// FLOWCRAFT_QWEN unset; FLOWCRAFT_TEST_QWEN set => use the test
	// var. This is the convention that lets a single repo-root .env
	// drive both conformance tests and eval CLIs.
	t.Setenv("FLOWCRAFT_TEST_QWEN", `{"api_key":"sk-q","model":"qwen-max"}`)

	_, model, cfg, err := Load("qwen")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if model != "qwen-max" {
		t.Errorf("model = %q, want qwen-max", model)
	}
	if cfg["api_key"] != "sk-q" {
		t.Errorf("api_key not loaded: %v", cfg)
	}
}

func TestLoad_FlowcraftBeatsTest(t *testing.T) {
	// Both set => FLOWCRAFT_<PROVIDER> wins.
	t.Setenv("FLOWCRAFT_QWEN", `{"api_key":"sk-prod","model":"qwen-max"}`)
	t.Setenv("FLOWCRAFT_TEST_QWEN", `{"api_key":"sk-test","model":"qwen-flash"}`)

	_, model, cfg, err := Load("qwen")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if model != "qwen-max" {
		t.Errorf("model = %q, want qwen-max (FLOWCRAFT_ should win)", model)
	}
	if cfg["api_key"] != "sk-prod" {
		t.Errorf("api_key = %v, want sk-prod", cfg["api_key"])
	}
}

func TestLoad_ModelOverrideFromSpec(t *testing.T) {
	t.Setenv("FLOWCRAFT_QWEN", `{"api_key":"sk-q","model":"qwen-max"}`)

	_, model, _, err := Load("qwen:qwen-flash")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if model != "qwen-flash" {
		t.Errorf("model = %q, want qwen-flash (spec :model should override)", model)
	}
}

func TestLoad_ModelInSpecOnly(t *testing.T) {
	// JSON has no "model" — spec :model is the only source.
	t.Setenv("FLOWCRAFT_QWEN", `{"api_key":"sk-q"}`)

	_, model, _, err := Load("qwen:qwen-flash")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if model != "qwen-flash" {
		t.Errorf("model = %q, want qwen-flash", model)
	}
}

func TestLoad_NoModel_Errors(t *testing.T) {
	t.Setenv("FLOWCRAFT_QWEN", `{"api_key":"sk-q"}`)

	_, _, _, err := Load("qwen")
	if err == nil {
		t.Fatal("expected error when neither JSON nor spec carries a model")
	}
	if !strings.Contains(err.Error(), "no \"model\"") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoad_NoCreds_Errors(t *testing.T) {
	// Make sure both vars are empty for this provider.
	t.Setenv("FLOWCRAFT_NOSUCH", "")
	t.Setenv("FLOWCRAFT_TEST_NOSUCH", "")

	_, _, _, err := Load("nosuch")
	if err == nil {
		t.Fatal("expected error when no env var is set")
	}
	if !strings.Contains(err.Error(), "no credentials") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoad_InvalidJSON_Errors(t *testing.T) {
	t.Setenv("FLOWCRAFT_QWEN", "{not-json}")

	_, _, _, err := Load("qwen")
	if err == nil {
		t.Fatal("expected JSON parse error")
	}
	if !strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoad_MissingAPIKey_Errors(t *testing.T) {
	t.Setenv("FLOWCRAFT_QWEN", `{"model":"qwen-max"}`)

	_, _, _, err := Load("qwen")
	if err == nil {
		t.Fatal("expected missing api_key error")
	}
	if !strings.Contains(err.Error(), "api_key") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoad_EmptySpec_Errors(t *testing.T) {
	_, _, _, err := Load("")
	if err == nil {
		t.Fatal("expected error for empty spec")
	}
}

func TestLoad_AliasResolvesToFactoryProvider(t *testing.T) {
	// Alias names a profile, JSON.provider names the factory. Two
	// aliases (azure_reasoning + azure_fast) can both resolve to the
	// "azure" factory with different api_key/caps so a single eval
	// run can use o1-mini for extraction and gpt-4o-mini for QA.
	t.Setenv("FLOWCRAFT_AZURE_REASONING", `{
		"provider":"azure",
		"api_key":"sk-r",
		"model":"o1-mini",
		"caps":{"no_temperature":true}
	}`)
	t.Setenv("FLOWCRAFT_AZURE_FAST", `{
		"provider":"azure",
		"api_key":"sk-f",
		"model":"gpt-4o-mini"
	}`)

	provR, modelR, cfgR, err := Load("azure_reasoning")
	if err != nil {
		t.Fatalf("Load reasoning: %v", err)
	}
	if provR != "azure" || modelR != "o1-mini" {
		t.Errorf("reasoning: got (%q, %q), want (azure, o1-mini)", provR, modelR)
	}
	if cfgR["api_key"] != "sk-r" {
		t.Errorf("reasoning api_key = %v", cfgR["api_key"])
	}
	if caps, ok := cfgR["caps"].(map[string]any); !ok || caps["no_temperature"] != true {
		t.Errorf("reasoning caps not preserved: %v", cfgR["caps"])
	}

	provF, modelF, cfgF, err := Load("azure_fast")
	if err != nil {
		t.Fatalf("Load fast: %v", err)
	}
	if provF != "azure" || modelF != "gpt-4o-mini" {
		t.Errorf("fast: got (%q, %q), want (azure, gpt-4o-mini)", provF, modelF)
	}
	if cfgF["api_key"] != "sk-f" {
		t.Errorf("fast api_key = %v", cfgF["api_key"])
	}
	if _, present := cfgF["caps"]; present {
		t.Errorf("fast caps should be empty, got %v", cfgF["caps"])
	}
}

func TestLoad_AliasFallsBackToProviderField(t *testing.T) {
	// When the alias matches a registered provider name (e.g. "qwen"),
	// the JSON does not need to repeat "provider":"qwen".
	t.Setenv("FLOWCRAFT_QWEN", `{"api_key":"sk-q","model":"qwen-max"}`)

	provider, _, _, err := Load("qwen")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if provider != "qwen" {
		t.Errorf("provider = %q, want qwen (alias fallback)", provider)
	}
}

func TestBuildLLM_EmptySpecReturnsNil(t *testing.T) {
	got, err := BuildLLM("")
	if err != nil {
		t.Fatalf("BuildLLM(\"\"): %v", err)
	}
	if got != nil {
		t.Errorf("BuildLLM(\"\") = %v, want nil", got)
	}
}

func TestBuildEmbedder_EmptySpecReturnsNil(t *testing.T) {
	got, err := BuildEmbedder("")
	if err != nil {
		t.Fatalf("BuildEmbedder(\"\"): %v", err)
	}
	if got != nil {
		t.Errorf("BuildEmbedder(\"\") = %v, want nil", got)
	}
}
