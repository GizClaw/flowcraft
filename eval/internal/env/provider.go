// Package env loads provider credentials from JSON-encoded env vars
// and resolves "<alias>[:<model>]" specs into the (provider, model,
// config) triple consumed by sdk/llm.NewFromConfig and
// sdkx/embedding.NewFromConfig.
//
// It keeps eval CLIs and tests from each carrying a verbatim env-var loader.
// The shape of the config map mirrors sdk/llm.NewFromConfig's signature
// so eval CLIs and tests/conformance/llm consume the same JSON layout.
//
// JSON shape (FLOWCRAFT_<ALIAS> / FLOWCRAFT_TEST_<ALIAS>):
//
//	{
//	  "provider":    "azure",            // optional; alias is used when absent
//	  "api_key":     "sk-...",
//	  "model":       "gpt-5.4",          // optional; flag :model wins
//	  "base_url":    "https://...",      // optional
//	  "api_version": "2024-08-01-preview", // optional, azure-style
//	  "caps": {                            // optional cap overrides
//	    "no_temperature":  true,
//	    "no_json_schema":  false,
//	    "no_json_mode":    false
//	  }
//	}
//
// Alias vs provider:
//
// The token before the ":" in the spec is treated as an *alias* — it
// names the env var, not the factory. This lets one provider expose
// several connection profiles (different api_key, base_url, caps) by
// registering distinct aliases. The actual factory name comes from
// the JSON "provider" field; if absent the alias itself is used.
//
//	FLOWCRAFT_AZURE_REASONING = {"provider":"azure", "api_key":..., "caps":{"no_temperature":true}}
//	FLOWCRAFT_AZURE_FAST      = {"provider":"azure", "api_key":..., "caps":{}}
//
//	--answer-llm azure_fast:gpt-4o-mini       # uses FLOWCRAFT_AZURE_FAST
//	--judge-llm  azure_reasoning:o1-mini      # uses FLOWCRAFT_AZURE_REASONING
//
// In the simple "one profile per provider" case the alias is just the
// provider name and the JSON's "provider" field can be omitted.
//
// Lookup priority (first non-empty wins):
//
//  1. FLOWCRAFT_<ALIAS>       — JSON blob; production-flavoured name.
//  2. FLOWCRAFT_TEST_<ALIAS>  — same JSON; reuses the env var the
//     conformance suite already populates so a single repo-root .env
//     can drive both eval CLIs and conformance tests.
package env

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

// Load parses spec and returns the provider name, model, and config map
// ready for sdk/llm.NewFromConfig or sdkx/embedding.NewFromConfig.
//
// spec is "<alias>" or "<alias>:<model>". The alias selects the env
// var (FLOWCRAFT_<ALIAS> upper-cased); the factory name is taken from
// the JSON "provider" field, defaulting to the alias when absent.
//
// When :model is omitted the model is taken from the JSON blob's
// "model" field; if neither supplies one Load returns an error.
func Load(spec string) (provider, model string, cfg map[string]any, err error) {
	alias, modelOverride, _ := strings.Cut(spec, ":")
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return "", "", nil, fmt.Errorf("expected <alias>[:<model>], got %q", spec)
	}
	envSuffix := strings.ToUpper(alias)
	model = modelOverride

	blob := firstNonEmpty(
		os.Getenv("FLOWCRAFT_"+envSuffix),
		os.Getenv("FLOWCRAFT_TEST_"+envSuffix),
	)
	if blob == "" {
		return "", "", nil, fmt.Errorf(
			"no credentials for alias %q: set FLOWCRAFT_%s or FLOWCRAFT_TEST_%s as a JSON blob",
			alias, envSuffix, envSuffix,
		)
	}

	var raw map[string]any
	if jerr := json.Unmarshal([]byte(blob), &raw); jerr != nil {
		return "", "", nil, fmt.Errorf("FLOWCRAFT_%s: invalid JSON: %w", envSuffix, jerr)
	}

	// Factory name: JSON "provider" wins, alias is the fallback so the
	// trivial case (alias = provider name) needs no extra fields.
	if v, ok := raw["provider"].(string); ok && v != "" {
		provider = v
	} else {
		provider = alias
	}
	delete(raw, "provider")

	if _, ok := raw["api_key"].(string); !ok {
		return "", "", nil, fmt.Errorf("FLOWCRAFT_%s: api_key missing or not a string", envSuffix)
	}
	if model == "" {
		if v, ok := raw["model"].(string); ok {
			model = v
		}
	}
	if model == "" {
		return "", "", nil, fmt.Errorf(
			"FLOWCRAFT_%s has no \"model\" and spec %q has no :model — pass <alias>:<model>",
			envSuffix, spec,
		)
	}
	// Strip "model" from the cfg map: factories receive the model
	// through the model parameter, not the config map. Keeping it
	// would be ignored by every existing factory but makes
	// inspection-time debugging needlessly noisy.
	delete(raw, "model")

	return provider, model, raw, nil
}

// BuildLLM resolves spec via Load and constructs an LLM through the
// global provider registry, then wraps it with the model's catalog
// caps + any user-supplied overrides from cfg["caps"]. Returns
// (nil, nil) when spec is empty so callers can pass an unset flag
// through unchanged.
//
// Why the WithCaps wrap: sdk/llm.NewFromConfig deliberately returns
// a *bare* provider connection (no caps / defaults / limits middleware)
// and pushes that responsibility onto the resolver. Eval CLIs do not
// go through DefaultResolver, so without this wrap the openai-compatible
// adapter happily forwards a `response_format: {"type":"json_schema"}`
// payload to providers like DeepSeek that only support `json_object`,
// and every Generate call comes back 400.
func BuildLLM(spec string) (llm.LLM, error) {
	if spec == "" {
		return nil, nil
	}
	provider, model, cfg, err := Load(spec)
	if err != nil {
		return nil, err
	}

	// Extract user caps overrides from cfg before handing it to the
	// factory; factories ignore "caps" but leaving it in is noisy.
	userCaps, _ := cfg["caps"].(map[string]any)
	delete(cfg, "caps")

	raw, err := llm.NewFromConfig(provider, model, cfg)
	if err != nil {
		return nil, err
	}
	caps := mergeCaps(llm.DefaultRegistry.LookupModelSpec(provider, model).Caps, userCaps)
	return llm.WithCaps(raw, caps), nil
}

// BuildEmbedder mirrors BuildLLM for the embedding registry.
func BuildEmbedder(spec string) (embedding.Embedder, error) {
	if spec == "" {
		return nil, nil
	}
	provider, model, cfg, err := Load(spec)
	if err != nil {
		return nil, err
	}
	return embedding.NewFromConfig(provider, model, cfg)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// userCapsKeys maps the env-friendly "no_<feature>" knobs to the
// canonical Capability constants. Only knobs we actually expose to
// eval users are listed; new entries can be added as needed.
var userCapsKeys = map[string]llm.Capability{
	"no_temperature":       llm.CapTemperature,
	"no_top_p":             llm.CapTopP,
	"no_top_k":             llm.CapTopK,
	"no_max_tokens":        llm.CapMaxTokens,
	"no_stop_words":        llm.CapStopWords,
	"no_frequency_penalty": llm.CapFrequencyPenalty,
	"no_presence_penalty":  llm.CapPresencePenalty,
	"no_thinking":          llm.CapThinking,
	"no_json_schema":       llm.CapJSONSchema,
	"no_json_mode":         llm.CapJSONMode,
	"no_tools":             llm.CapTools,
	"no_tool_choice":       llm.CapToolChoice,
	"no_parallel_tools":    llm.CapParallelTools,
	"no_streaming":         llm.CapStreaming,
	"no_system_prompt":     llm.CapSystemPrompt,
	"no_vision":            llm.CapVision,
	"no_audio":             llm.CapAudio,
	"no_file":              llm.CapFile,
}

// mergeCaps unions the catalog's intrinsic caps (e.g. DeepSeek
// declaring CapJSONSchema disabled in its model spec) with user
// overrides from FLOWCRAFT_<ALIAS>.caps, so per-deployment quirks
// (e.g. a custom Azure deployment that rejects temperature) can be
// declared at the env layer without patching the SDK catalog.
func mergeCaps(catalog llm.ModelCaps, user map[string]any) llm.ModelCaps {
	if len(user) == 0 {
		return catalog
	}
	merged := llm.ModelCaps{Disabled: map[llm.Capability]bool{}}
	for k, v := range catalog.Disabled {
		merged.Disabled[k] = v
	}
	for key, raw := range user {
		on, _ := raw.(bool)
		if !on {
			continue
		}
		if cap, ok := userCapsKeys[key]; ok {
			merged.Disabled[cap] = true
		}
	}
	return merged
}
