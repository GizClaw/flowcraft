package anthropic

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/resource"
)

// Spec is the provider-level configuration.
type Spec struct {
	// Endpoint locates the Messages API. Transport only.
	Endpoint EndpointSpec `json:"endpoint,omitempty"`
	// Catalog selects the model namespace: "builtin_declared" (default)
	// merges spec.models over the built-in Claude line-up, "declared" starts
	// from an empty catalog so a compatible endpoint's model names never
	// inherit Claude facts.
	Catalog string `json:"catalog,omitempty"`
	// Wire declares provider extensions this endpoint accepts on top of the
	// Messages protocol. Empty leaves every extension off.
	Wire WireSpec `json:"wire,omitempty"`
	// HTTPRetries bounds wire-level retries inside one logical inference
	// attempt, including the first.
	HTTPRetries *resource.Int `json:"http_retries,omitempty"`
	// Models declares custom models or overrides built-in catalog entries.
	Models []ModelSpec `json:"models,omitempty"`
}

// EndpointSpec locates one Messages API endpoint.
type EndpointSpec struct {
	// BaseURL overrides the API origin, e.g. for a gateway or a compatible
	// endpoint such as MiniMax's /anthropic surface.
	BaseURL string `json:"base_url,omitempty"`
}

// WireSpec declares provider extensions an endpoint accepts on top of the
// Messages protocol. Each one is gated twice: here, and by the model's own
// capability declaration, so an extension can never be sent on behalf of a
// model that does not claim it.
type WireSpec struct {
	// VideoInput allows video content blocks, a compatible-endpoint
	// extension: Anthropic's own schema has no video block.
	VideoInput bool `json:"video_input,omitempty"`
}

// catalogMode selects which model namespace one provider instance serves.
type catalogMode string

const (
	// catalogBuiltinDeclared merges spec.models over the built-in line-up.
	catalogBuiltinDeclared catalogMode = "builtin_declared"
	// catalogDeclared starts from an empty catalog.
	catalogDeclared catalogMode = "declared"
)

// catalogMode returns the normalized catalog mode.
func (s Spec) catalogMode() catalogMode {
	if s.Catalog == "" {
		return catalogBuiltinDeclared
	}
	return catalogMode(s.Catalog)
}

// ModelSpec declares one catalog overlay entry: a leaf-level patch over the
// same-named built-in model (or a fresh declaration for unknown names).
// Capability leaves the entry does not name are inherited from the built-in
// entry, so tweaking one channel cannot silently revoke the rest.
type ModelSpec struct {
	Name string `json:"name"`
	// Kind declares the operation family. Messages models generate text, so
	// "generate" is the default; any other kind is rejected because the
	// Messages protocol has no channel for it.
	Kind string `json:"kind,omitempty"`
	// Capabilities declares the capability leaves this model changes.
	Capabilities *model.CapabilitiesPatch `json:"capabilities,omitempty"`
	// Limits declares numeric capacity limits for the model. Overriding a
	// built-in catalog entry by name keeps the catalog limit for any field
	// left nil; declaring a value replaces it.
	Limits model.ModelLimits `json:"limits,omitempty"`
}

func (s Spec) Validate() error {
	if s.Endpoint.BaseURL != "" &&
		!strings.HasPrefix(s.Endpoint.BaseURL, "https://") &&
		!strings.HasPrefix(s.Endpoint.BaseURL, "http://") {
		return fmt.Errorf(
			"anthropic: endpoint.base_url %q must be an http(s) URL",
			s.Endpoint.BaseURL,
		)
	}
	switch s.catalogMode() {
	case catalogBuiltinDeclared, catalogDeclared:
	default:
		return fmt.Errorf("anthropic: catalog must be \"builtin_declared\" or \"declared\"")
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
		if model.Kind != "" && model.Kind != "generate" {
			return fmt.Errorf(
				"anthropic: model %q kind %q is not served by the Messages protocol",
				model.Name,
				model.Kind,
			)
		}
		if err := model.Capabilities.Validate(); err != nil {
			return fmt.Errorf("anthropic: model %q: %w", model.Name, err)
		}
		if model.Capabilities != nil &&
			model.Capabilities.CustomEmbedDimensions != nil {
			return fmt.Errorf(
				"anthropic: model %q: custom_embed_dimensions is unsupported "+
					"(anthropic serves text generation only)",
				model.Name,
			)
		}
		if err := model.Limits.Validate(); err != nil {
			return fmt.Errorf("anthropic: model %q: %w", model.Name, err)
		}
	}
	return nil
}

// ProfileSpec carries no profile-scoped settings today.
type ProfileSpec struct{}

func (ProfileSpec) Validate() error { return nil }

func decodeSpec(ctx context.Context, raw []byte) (Spec, error) {
	spec, err := resource.DecodeTyped[Spec](ctx, raw)
	if err != nil {
		return Spec{}, fmt.Errorf("anthropic spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, fmt.Errorf("anthropic spec: %w", err)
	}
	return spec, nil
}
