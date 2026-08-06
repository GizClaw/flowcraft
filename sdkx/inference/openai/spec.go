package openai

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference/config"
)

// Secret names owned by this provider. Profile secrets outside this set are
// rejected at build time so typos fail fast instead of silently missing.
const (
	// SecretAPIKey authenticates every OpenAI API surface.
	SecretAPIKey = "api_key"
)

// Spec is the provider-level configuration for OpenAI. It must stay
// credential-free: config.DecodeSpec already rejects credential-shaped keys.
type Spec struct {
	// BaseURL overrides the API base URL (gateways, proxies, Azure-style
	// compatible endpoints).
	BaseURL string `json:"base_url,omitempty"`
	// Organization sets the OpenAI-Organization header.
	Organization string `json:"organization,omitempty"`
	// Project sets the OpenAI-Project header.
	Project string `json:"project,omitempty"`
	// Models declares additional models beyond the built-in catalog or
	// overrides catalog entries by name.
	Models []ModelSpec `json:"models,omitempty"`
}

// ModelSpec declares one model outside the built-in catalog. Capability
// flags are only meaningful for the matching kind.
type ModelSpec struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Vision (generate) allows image input parts.
	Vision bool `json:"vision,omitempty"`
	// Reasoning (generate) enables the reasoning effort control.
	Reasoning bool `json:"reasoning,omitempty"`
	// Dimensions (embed) allows custom output dimensions.
	Dimensions bool `json:"dimensions,omitempty"`
}

// ProfileSpec is the per-credential-profile configuration. OpenAI addresses
// models by public slug and every surface shares one API key, so no
// profile-scoped settings exist today; the struct is reserved so future
// account-scoped settings have a home without a config schema break.
type ProfileSpec struct{}

func (s ProfileSpec) Validate() error {
	return nil
}

var modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (s Spec) Validate() error {
	if s.BaseURL != "" &&
		!strings.HasPrefix(s.BaseURL, "https://") &&
		!strings.HasPrefix(s.BaseURL, "http://") {
		return fmt.Errorf("base_url must be an http(s) URL")
	}
	seen := make(map[string]struct{}, len(s.Models))
	for index, model := range s.Models {
		if err := model.Validate(); err != nil {
			return fmt.Errorf("models[%d]: %w", index, err)
		}
		if _, duplicate := seen[model.Name]; duplicate {
			return fmt.Errorf("models[%d]: duplicate model %q", index, model.Name)
		}
		seen[model.Name] = struct{}{}
	}
	return nil
}

func (m ModelSpec) Validate() error {
	if !modelNamePattern.MatchString(m.Name) {
		return fmt.Errorf("invalid model name %q", m.Name)
	}
	switch modelKind(m.Kind) {
	case kindGenerate, kindEmbed, kindImage, kindTTS, kindASR:
	default:
		return fmt.Errorf("model %q has unknown kind %q", m.Name, m.Kind)
	}
	return nil
}

func decodeSpec(raw []byte) (Spec, error) {
	spec, err := config.DecodeSpec[Spec](raw)
	if err != nil {
		return Spec{}, fmt.Errorf("openai spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, fmt.Errorf("openai spec: %w", err)
	}
	return spec, nil
}

func decodeProfileSpec(raw []byte) (ProfileSpec, error) {
	spec, err := config.DecodeSpec[ProfileSpec](raw)
	if err != nil {
		return ProfileSpec{}, fmt.Errorf("openai profile spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return ProfileSpec{}, fmt.Errorf("openai profile spec: %w", err)
	}
	return spec, nil
}
