package anthropic

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/resource"
)

// Spec is the provider-level configuration.
type Spec struct {
	// Endpoint locates the Messages API. Transport only.
	Endpoint EndpointSpec `json:"endpoint,omitempty"`
	// Catalog is retired and carries no behavior: the driver ships no model
	// line-up, so every model a deployment serves is declared in Models. Any
	// value is rejected with the migration path rather than silently
	// accepted, because the key used to select a namespace that no longer
	// exists.
	Catalog string `json:"catalog,omitempty"`
	// Wire declares provider extensions this endpoint accepts on top of the
	// Messages protocol. Empty leaves every extension off.
	Wire WireSpec `json:"wire,omitempty"`
	// HTTPRetries bounds wire-level retries inside one logical inference
	// attempt, including the first.
	HTTPRetries *resource.Int `json:"http_retries,omitempty"`
	// Models declares the line-up this deployment serves. It is the only
	// source of models: the driver ships none, and nothing is inherited.
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
	// ReasoningScope declares the verification scope this deployment's
	// thinking traces belong to. It replaces the derived scope — provider,
	// model, and credential profile — so an operator who has verified that
	// several models or credentials accept each other's signed traces says so
	// once. Unset keeps the conservative derived scope, which never replays a
	// trace across a model or an account; Anthropic verifies thinking
	// signatures per model, so that default is what the API requires.
	ReasoningScope string `json:"reasoning_scope,omitempty"`
}

// ModelSpec declares one model this deployment serves. The driver ships no
// line-up, so the declaration is complete: what it states is what the model
// promises, and a leaf it leaves out is undeclared rather than inherited.
type ModelSpec struct {
	Name string `json:"name"`
	// Kind declares the operation family. Messages models generate text, so
	// "generate" is the default; any other kind is rejected because the
	// Messages protocol has no channel for it.
	Kind string `json:"kind,omitempty"`
	// Capabilities declares what this model accepts and produces.
	Capabilities model.ModelCapabilities `json:"capabilities,omitempty"`
	// Limits declares the model's numeric capacity limits. Undeclared leaves
	// claim no bound rather than a zero one.
	Limits model.ModelLimits `json:"limits,omitempty"`
	// Lifecycle declares the model's discovery metadata: deprecation,
	// retirement time, and the model that replaces it. Empty means active.
	// The replacement is a full model identity, so it can name a model this
	// deployment serves or one served elsewhere; validation runs when the
	// model is published, where its own identity is known.
	Lifecycle model.ModelLifecycle `json:"lifecycle,omitzero"`
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
	if s.Catalog != "" {
		return fmt.Errorf(
			"anthropic: catalog %q is not supported: the driver ships no model "+
				"line-up, so every model must be declared in models (remove the key)",
			s.Catalog,
		)
	}
	if s.HTTPRetries != nil && *s.HTTPRetries < 0 {
		return fmt.Errorf("anthropic: http_retries must not be negative")
	}
	if err := validateReasoningScope(s.Wire.ReasoningScope); err != nil {
		return err
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
		if model.Capabilities.CustomEmbedDimensions {
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

// maxReasoningScopeLen bounds the declared scope token: it lands in a
// compile-report reason, so it stays short and printable.
const maxReasoningScopeLen = 128

// validateReasoningScope checks the declared verification scope. The token is
// opaque to the driver, but the comparison is exact, so surrounding
// whitespace and control characters are rejected rather than normalized.
func validateReasoningScope(scope string) error {
	if scope == "" {
		return nil
	}
	if strings.TrimSpace(scope) != scope {
		return fmt.Errorf(
			"anthropic: wire.reasoning_scope must not have surrounding whitespace")
	}
	if len(scope) > maxReasoningScopeLen {
		return fmt.Errorf(
			"anthropic: wire.reasoning_scope is %d bytes, at most %d are allowed",
			len(scope), maxReasoningScopeLen,
		)
	}
	for _, char := range scope {
		if unicode.IsControl(char) {
			return fmt.Errorf(
				"anthropic: wire.reasoning_scope must not contain control characters")
		}
	}
	return nil
}

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
