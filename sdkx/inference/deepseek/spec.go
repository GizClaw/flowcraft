package deepseek

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference/config"
)

// SecretAPIKey is the provider-owned secret name for the DeepSeek API key.
const SecretAPIKey = "api_key"

// Spec is the provider-level configuration surface. It is credential-free:
// secrets resolve per profile and never appear here.
type Spec struct {
	// BaseURL overrides the API endpoint. Defaults to
	// https://api.deepseek.com (the OpenAI-compatible surface; the
	// provider serves chat completions only, no Responses API).
	BaseURL string `json:"base_url,omitempty"`
	// HTTPRetries bounds wire-level retries inside one logical inference
	// attempt, including the first. Zero disables SDK-internal retries so
	// the route Router owns the full retry budget; nil keeps the openai-go
	// default (two retries).
	HTTPRetries *int `json:"http_retries,omitempty"`
	// Models declares models outside the built-in catalog or overrides
	// catalog entries by name.
	Models []ModelSpec `json:"models,omitempty"`
}

// ModelSpec declares one model the deployment serves. Kind is the only
// discriminator the compiler needs beyond the catalog defaults.
type ModelSpec struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Reasoning bool   `json:"reasoning,omitempty"`
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
	return nil
}

// Validate checks the provider spec for structural sanity.
func (s Spec) Validate() error {
	if s.BaseURL != "" {
		if err := validateURL("base_url", s.BaseURL); err != nil {
			return err
		}
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

// ProfileSpec is the profile-level configuration surface. DeepSeek scopes
// nothing per account today — the struct exists so deployments can attach
// profile ids for credential rotation without a schema change later.
type ProfileSpec struct{}

// Validate checks the profile spec. The empty surface always passes.
func (s ProfileSpec) Validate() error { return nil }

func validateURL(name, value string) error {
	if !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "http://") {
		return fmt.Errorf("%s must be an http(s) URL", name)
	}
	return nil
}

func decodeSpec(raw []byte) (Spec, error) {
	spec, err := config.DecodeSpec[Spec](raw)
	if err != nil {
		return Spec{}, err
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

func decodeProfileSpec(raw []byte) (ProfileSpec, error) {
	spec, err := config.DecodeSpec[ProfileSpec](raw)
	if err != nil {
		return ProfileSpec{}, err
	}
	if err := spec.Validate(); err != nil {
		return ProfileSpec{}, err
	}
	return spec, nil
}
