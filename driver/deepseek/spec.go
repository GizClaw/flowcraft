package deepseek

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

// SecretAPIKey is the provider-owned secret name for the DeepSeek API key.
const SecretAPIKey = "api_key"

// Spec is the provider-level configuration surface. It is credential-free:
// secrets resolve per profile and never appear here.
type Spec struct {
	// API selects the generate surface: "chat" (default) or "responses".
	// Built-in models without the capability are excluded from a
	// Responses provider, and declared models without it are rejected.
	API string `json:"api,omitempty"`
	// BaseURL overrides the API endpoint. Defaults to
	// https://api.deepseek.com (the OpenAI-compatible surface shared by
	// chat and responses).
	BaseURL string `json:"base_url,omitempty"`
	// HTTPRetries bounds wire-level retries inside one logical inference
	// attempt, including the first. Zero disables SDK-internal retries so
	// the route Router owns the full retry budget; nil keeps the openai-go
	// default (two retries).
	HTTPRetries *resource.Int `json:"http_retries,omitempty"`
	// Models declares models outside the built-in catalog or overrides
	// catalog entries by name.
	Models []ModelSpec `json:"models,omitempty"`
}

// ModelSpec declares one model the deployment serves. Capabilities mirror
// the built-in catalog shape: content kinds, hosted web search, and the
// reasoning control capability. Responses is a wire-surface fact and stays a
// separate flag.
type ModelSpec struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Capabilities declares the model's input/output content kinds, hosted
	// web search support, and reasoning control capability.
	Capabilities inference.ModelCapabilities `json:"capabilities,omitempty"`
	// Responses declares Responses API support (deepseek-v4-flash and
	// deepseek-v4-pro).
	Responses bool `json:"responses,omitempty"`
}

var modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Validate checks the model declaration for structural sanity.
func (m ModelSpec) Validate() error {
	if !modelNamePattern.MatchString(m.Name) {
		return fmt.Errorf("invalid model name %q", m.Name)
	}
	if m.Kind != "" && m.Kind != string(kindGenerate) {
		return fmt.Errorf("model %q declares unsupported kind %q", m.Name, m.Kind)
	}
	return m.Capabilities.Validate()
}

// Validate checks the provider spec for structural sanity.
func (s Spec) Validate() error {
	switch s.apiMode() {
	case apiChat, apiResponses:
	default:
		return fmt.Errorf("api must be \"chat\" or \"responses\"")
	}
	if s.BaseURL != "" &&
		!strings.HasPrefix(s.BaseURL, "https://") &&
		!strings.HasPrefix(s.BaseURL, "http://") {
		return fmt.Errorf("base_url must be an http(s) URL")
	}
	if s.HTTPRetries != nil && *s.HTTPRetries < 0 {
		return fmt.Errorf("http_retries must not be negative")
	}
	seen := make(map[string]struct{}, len(s.Models))
	for _, model := range s.Models {
		if err := model.Validate(); err != nil {
			return err
		}
		if _, exists := seen[model.Name]; exists {
			return fmt.Errorf("duplicate model declaration %q", model.Name)
		}
		seen[model.Name] = struct{}{}
	}
	return nil
}

// apiMode returns the normalized generate API mode.
func (s Spec) apiMode() apiMode {
	if s.API == "" {
		return apiChat
	}
	return apiMode(s.API)
}

// ProfileSpec is the profile-level configuration surface. DeepSeek scopes
// nothing per account today — the struct exists so deployments can attach
// profile ids for credential rotation without a schema change later.
type ProfileSpec struct{}

// Validate checks the profile spec. The empty surface always passes.
func (s ProfileSpec) Validate() error { return nil }

func decodeSpec(raw []byte) (Spec, error) {
	spec, err := resource.DecodeTyped[Spec](raw)
	if err != nil {
		return Spec{}, fmt.Errorf("deepseek spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, fmt.Errorf("deepseek spec: %w", err)
	}
	return spec, nil
}

func decodeProfileSpec(raw []byte) (ProfileSpec, error) {
	spec, err := resource.DecodeTyped[ProfileSpec](raw)
	if err != nil {
		return ProfileSpec{}, fmt.Errorf("deepseek profile spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return ProfileSpec{}, fmt.Errorf("deepseek profile spec: %w", err)
	}
	return spec, nil
}
