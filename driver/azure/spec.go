package azure

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

// DefaultAPIVersion is the api-version used when spec.api_version is unset.
const DefaultAPIVersion = "2025-04-01-preview"

// Spec is the provider-level configuration: one Azure OpenAI resource plus
// the deployments it exposes.
type Spec struct {
	// Endpoint is the resource URL, e.g. https://myresource.openai.azure.com.
	Endpoint string `json:"endpoint"`
	// APIVersion overrides the query api-version; defaults to
	// DefaultAPIVersion.
	APIVersion string `json:"api_version,omitempty"`
	// HTTPRetries bounds wire-level retries inside one logical inference
	// attempt, including the first.
	HTTPRetries *resource.Int `json:"http_retries,omitempty"`
	// RequestMetadata controls how canonical GenerateRequest metadata is
	// projected onto the provider request body. The empty value disables
	// forwarding; any non-empty value names the top-level body field that
	// receives the bag. "metadata" uses the native metadata object and
	// "client_metadata" uses the Codex-style passthrough, but other names
	// are allowed for gateways.
	RequestMetadata *RequestMetadataSpec `json:"request_metadata,omitempty"`
	// Models declares the deployments to expose; at least one is required.
	Models []ModelSpec `json:"models"`
}

// RequestMetadataSpec is the provider-level lowering policy for canonical
// GenerateRequest.RequestMetadata. Keys and the envelope are opaque and
// forwarded verbatim.
type RequestMetadataSpec struct {
	Envelope string `json:"envelope,omitempty"`
}

// Validate checks the request metadata forwarding policy.
func (s RequestMetadataSpec) Validate() error {
	return nil
}

// ModelSpec declares one deployment: its name is the model identity, kind
// selects the operation family, and the capability flags opt into channels
// beyond the bare text surface.
type ModelSpec struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // generate | embed | image | tts
	// Capabilities declares the deployment's input/output content kinds,
	// hosted web search support, and reasoning control capability, validated
	// against the kind's compiler contract.
	Capabilities inference.ModelCapabilities `json:"capabilities,omitempty"`
	// Dimensions accepts the embed dimensions knob (embed only).
	Dimensions bool `json:"dimensions,omitempty"`
	// EffortNone marks generate deployments whose reasoning.effort accepts
	// "none" to disable reasoning (gpt-5.1+ deployments); deployments
	// without it reject a ReasoningEnabled=false request.
	EffortNone bool `json:"effort_none,omitempty"`
}

func (s Spec) Validate() error {
	if s.Endpoint == "" {
		return fmt.Errorf("azure: endpoint is required")
	}
	if !strings.HasPrefix(s.Endpoint, "https://") &&
		!isLoopbackHTTP(s.Endpoint) {
		return fmt.Errorf(
			"azure: endpoint %q must be an https URL",
			s.Endpoint,
		)
	}
	if s.APIVersion != "" && strings.ContainsAny(s.APIVersion, "?&") {
		return fmt.Errorf("azure: api_version %q is not a version token", s.APIVersion)
	}
	if s.HTTPRetries != nil && *s.HTTPRetries < 0 {
		return fmt.Errorf("azure: http_retries must not be negative")
	}
	if s.RequestMetadata != nil {
		if err := s.RequestMetadata.Validate(); err != nil {
			return err
		}
	}
	if len(s.Models) == 0 {
		return fmt.Errorf(
			"azure: at least one deployment is required; azure has no built-in model catalog",
		)
	}
	seen := make(map[string]bool, len(s.Models))
	for _, model := range s.Models {
		if model.Name == "" || strings.ContainsAny(model.Name, " /") {
			return fmt.Errorf("azure: deployment name %q is not a valid token", model.Name)
		}
		if seen[model.Name] {
			return fmt.Errorf("azure: duplicate deployment %q", model.Name)
		}
		seen[model.Name] = true
		switch modelKind(model.Kind) {
		case kindGenerate, kindEmbed, kindImage, kindTTS:
		case kindASR:
			return fmt.Errorf(
				"azure: deployment %q kind %q is not supported by core inference yet",
				model.Name,
				model.Kind,
			)
		default:
			return fmt.Errorf(
				"azure: deployment %q has unknown kind %q (want generate|embed|image|tts)",
				model.Name,
				model.Kind,
			)
		}
		if model.Dimensions && modelKind(model.Kind) != kindEmbed {
			return fmt.Errorf(
				"azure: deployment %q sets dimensions on kind %q",
				model.Name,
				model.Kind,
			)
		}
		if err := entryFor(model).validate(); err != nil {
			return fmt.Errorf("azure: deployment %q: %w", model.Name, err)
		}
	}
	return nil
}

// isLoopbackHTTP permits plain HTTP only against loopback hosts.
func isLoopbackHTTP(endpoint string) bool {
	if !strings.HasPrefix(endpoint, "http://") {
		return false
	}
	host := strings.TrimPrefix(endpoint, "http://")
	if index := strings.IndexAny(host, "/:"); index >= 0 {
		host = host[:index]
	}
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// modelKind selects the operation family a deployment serves.
type modelKind string

const (
	kindGenerate modelKind = "generate"
	kindEmbed    modelKind = "embed"
	kindImage    modelKind = "image"
	kindTTS      modelKind = "tts"
	kindASR      modelKind = "asr"
)

// ProfileSpec carries no profile-scoped settings today.
type ProfileSpec struct{}

func (ProfileSpec) Validate() error { return nil }

func decodeSpec(ctx context.Context, raw []byte) (Spec, error) {
	spec, err := resource.DecodeTyped[Spec](ctx, raw)
	if err != nil {
		return Spec{}, fmt.Errorf("azure spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, fmt.Errorf("azure spec: %w", err)
	}
	return spec, nil
}

func (s Spec) requestMetadataEnvelope() string {
	if s.RequestMetadata == nil {
		return ""
	}
	return s.RequestMetadata.Envelope
}
