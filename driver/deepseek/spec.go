package deepseek

import (
	"context"
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
	// Every catalog model is served on the selected surface; the official
	// DeepSeek line-up supports both (see
	// https://api-docs.deepseek.com/guides/responses_api).
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
	// RequestMetadata controls how canonical GenerateRequest metadata is
	// projected onto the provider request body. The empty value disables
	// forwarding entirely (the official DeepSeek API does not accept
	// request metadata); any non-empty value names the top-level body
	// field that receives the bag. "metadata" is the OpenAI-compatible
	// typed metadata object and "client_metadata" is the Codex-style
	// passthrough, but deployments may use other names for gateways.
	RequestMetadata *RequestMetadataSpec `json:"request_metadata,omitempty"`
	// Models declares models outside the built-in catalog or overrides
	// catalog entries by name.
	Models []ModelSpec `json:"models,omitempty"`
}

// RequestMetadataSpec is the provider-level lowering policy for canonical
// GenerateRequest.RequestMetadata. Core keys and the envelope name are
// opaque; the driver forwards the whole bag without interpreting entries.
type RequestMetadataSpec struct {
	// Envelope names the top-level body field that receives the metadata
	// object. "" disables forwarding; any non-empty string is accepted.
	Envelope string `json:"envelope,omitempty"`
}

// Validate checks the request metadata forwarding policy.
func (s RequestMetadataSpec) Validate() error {
	return nil
}

// ModelSpec declares one model the deployment serves, or a delta over a
// same-named built-in catalog entry. Capabilities is a patch with
// field-presence semantics: leaves it names replace that leaf of the entry
// it overrides and leaves it does not name are inherited. The generate
// surface is provider-wide (Spec.API), not per-model.
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
			"model %q declares custom_embed_dimensions, but deepseek "+
				"serves text generation only",
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
	if s.RequestMetadata != nil {
		if err := s.RequestMetadata.Validate(); err != nil {
			return err
		}
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

// requestMetadataEnvelope returns the configured forwarding envelope, or ""
// when forwarding is disabled.
func (s Spec) requestMetadataEnvelope() string {
	if s.RequestMetadata == nil {
		return ""
	}
	return s.RequestMetadata.Envelope
}

// ProfileSpec is the profile-level configuration surface. DeepSeek scopes
// nothing per account today — the struct exists so deployments can attach
// profile ids for credential rotation without a schema change later.
type ProfileSpec struct{}

// Validate checks the profile spec. The empty surface always passes.
func (s ProfileSpec) Validate() error { return nil }

func decodeSpec(ctx context.Context, raw []byte) (Spec, error) {
	spec, err := resource.DecodeTyped[Spec](ctx, raw)
	if err != nil {
		return Spec{}, fmt.Errorf("deepseek spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, fmt.Errorf("deepseek spec: %w", err)
	}
	return spec, nil
}

func decodeProfileSpec(ctx context.Context, raw []byte) (ProfileSpec, error) {
	spec, err := resource.DecodeTyped[ProfileSpec](ctx, raw)
	if err != nil {
		return ProfileSpec{}, fmt.Errorf("deepseek profile spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return ProfileSpec{}, fmt.Errorf("deepseek profile spec: %w", err)
	}
	return spec, nil
}
