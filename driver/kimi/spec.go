package kimi

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

// SecretAPIKey is the provider-owned secret name for the Moonshot API key.
const SecretAPIKey = "api_key"

// Spec is the provider-level configuration surface. It is credential-free:
// secrets resolve per profile and never appear here.
type Spec struct {
	// BaseURL overrides the API endpoint root. Defaults to
	// https://api.moonshot.cn/v1 (the OpenAI-compatible surface; chat
	// completions only).
	BaseURL string `json:"base_url,omitempty"`
	// HTTPRetries bounds wire-level HTTP retries inside one logical
	// inference attempt. Zero disables transport retries so the route
	// Router owns the full retry budget; nil keeps the httpkit default.
	HTTPRetries *resource.Int `json:"http_retries,omitempty"`
	// Models declares models outside the built-in catalog or extends
	// catalog entries by name. Same-name entries are leaf-level patches
	// over the built-in: written leaves replace, unstated leaves inherit.
	Models []ModelSpec `json:"models,omitempty"`
}

// ModelSpec declares one model the deployment serves, or a delta over a
// same-named built-in catalog entry. Capabilities is a patch with
// field-presence semantics: leaves it names replace that leaf of the entry
// it overrides and leaves it does not name are inherited. Kimi serves text
// generation only.
type ModelSpec struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Capabilities declares the capability leaves this model changes.
	Capabilities *inference.CapabilitiesPatch `json:"capabilities,omitempty"`
	// Limits declares numeric capacity limits for the model. Overriding a
	// built-in catalog entry by name keeps the catalog limit for any field
	// left nil; declaring a value replaces it.
	Limits inference.ModelLimits `json:"limits,omitempty"`
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
	if m.Capabilities != nil &&
		m.Capabilities.CustomEmbedDimensions != nil &&
		*m.Capabilities.CustomEmbedDimensions {
		return fmt.Errorf(
			"model %q declares custom_embed_dimensions, but kimi serves "+
				"text generation only",
			m.Name,
		)
	}
	if err := m.Capabilities.Validate(); err != nil {
		return err
	}
	return m.Limits.Validate()
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

// ProfileSpec is the profile-level configuration surface. Kimi scopes
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

func decodeSpec(ctx context.Context, raw []byte) (Spec, error) {
	spec, err := resource.DecodeTyped[Spec](ctx, raw)
	if err != nil {
		return Spec{}, fmt.Errorf("kimi spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, fmt.Errorf("kimi spec: %w", err)
	}
	return spec, nil
}

func decodeProfileSpec(ctx context.Context, raw []byte) (ProfileSpec, error) {
	spec, err := resource.DecodeTyped[ProfileSpec](ctx, raw)
	if err != nil {
		return ProfileSpec{}, fmt.Errorf("kimi profile spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return ProfileSpec{}, fmt.Errorf("kimi profile spec: %w", err)
	}
	return spec, nil
}
