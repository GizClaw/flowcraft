package anthropic

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/resource"
)

// Spec is the provider-level configuration.
type Spec struct {
	// BaseURL overrides the API origin, e.g. for a gateway.
	BaseURL string `json:"base_url,omitempty"`
	// HTTPRetries bounds wire-level retries inside one logical inference
	// attempt, including the first.
	HTTPRetries *int `json:"http_retries,omitempty"`
	// Models declares custom models or overrides built-in catalog entries.
	Models []ModelSpec `json:"models,omitempty"`
}

// ModelSpec declares one catalog overlay entry.
type ModelSpec struct {
	Name string `json:"name"`
	// Vision accepts image input parts.
	Vision bool `json:"vision,omitempty"`
	// Reasoning accepts the reasoning effort knob.
	Reasoning bool `json:"reasoning,omitempty"`
}

func (s Spec) Validate() error {
	if s.BaseURL != "" &&
		!strings.HasPrefix(s.BaseURL, "https://") &&
		!strings.HasPrefix(s.BaseURL, "http://") {
		return fmt.Errorf("anthropic: base_url %q must be an http(s) URL", s.BaseURL)
	}
	if s.HTTPRetries != nil && *s.HTTPRetries < 0 {
		return fmt.Errorf("anthropic: http_retries must not be negative")
	}
	seen := make(map[string]bool, len(s.Models))
	for _, model := range s.Models {
		if model.Name == "" || strings.ContainsAny(model.Name, " /") {
			return fmt.Errorf(
				"anthropic: model name %q is not a valid token",
				model.Name,
			)
		}
		if seen[model.Name] {
			return fmt.Errorf("anthropic: duplicate model %q", model.Name)
		}
		seen[model.Name] = true
	}
	return nil
}

// ProfileSpec carries no profile-scoped settings today.
type ProfileSpec struct{}

func (ProfileSpec) Validate() error { return nil }

func decodeSpec(raw []byte) (Spec, error) {
	spec, err := resource.DecodeTyped[Spec](raw)
	if err != nil {
		return Spec{}, fmt.Errorf("anthropic spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, fmt.Errorf("anthropic spec: %w", err)
	}
	return spec, nil
}
