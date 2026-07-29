package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Spec is the provider-level configuration.
type Spec struct {
	// BaseURL overrides the API origin, e.g. for a gateway. Empty uses the
	// SDK default (https://api.anthropic.com).
	BaseURL string `json:"base_url,omitempty"`
	// Models declares custom models or overrides built-in catalog entries.
	Models []ModelSpec `json:"models,omitempty"`
}

// ModelSpec declares one catalog overlay entry. Claude models are all
// generate kind, so kind is fixed and omitted from the schema.
type ModelSpec struct {
	Name string `json:"name"`
	// Vision accepts image input parts. Default off for custom models.
	Vision bool `json:"vision,omitempty"`
	// Reasoning accepts the reasoning effort knob. Default off for custom
	// models.
	Reasoning bool `json:"reasoning,omitempty"`
}

func (s Spec) Validate() error {
	if s.BaseURL != "" &&
		!strings.HasPrefix(s.BaseURL, "https://") &&
		!strings.HasPrefix(s.BaseURL, "http://") {
		return fmt.Errorf("anthropic: base_url %q must be an http(s) URL", s.BaseURL)
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

func decodeSpec(raw []byte) (Spec, error) {
	var spec Spec
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return Spec{}, fmt.Errorf("anthropic: provider spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

func decodeProfileSpec(raw []byte) (ProfileSpec, error) {
	var spec ProfileSpec
	if len(raw) == 0 || string(raw) == "null" {
		return spec, nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return ProfileSpec{}, fmt.Errorf("anthropic: profile spec: %w", err)
	}
	return spec, nil
}
