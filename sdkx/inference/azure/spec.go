package azure

import (
	"encoding/json"
	"fmt"
	"strings"
)

// DefaultAPIVersion is the api-version used when spec.api_version is unset:
// the first preview carrying the Responses API, gpt-image generation, and
// gpt-4o transcription together. Deployments pinned to another version set
// api_version explicitly.
const DefaultAPIVersion = "2025-04-01-preview"

// Spec is the provider-level configuration: one Azure OpenAI resource plus
// the deployments it exposes.
type Spec struct {
	// Endpoint is the resource URL, e.g. https://myresource.openai.azure.com.
	Endpoint string `json:"endpoint"`
	// APIVersion overrides the query api-version; defaults to
	// DefaultAPIVersion.
	APIVersion string `json:"api_version,omitempty"`
	// Models declares the deployments to expose; at least one is required.
	Models []ModelSpec `json:"models"`
}

// ModelSpec declares one deployment: its name is the model identity, kind
// selects the operation family, and the capability flags opt into channels
// beyond the bare text surface.
type ModelSpec struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // generate | embed | image | tts | asr
	// Vision accepts image input parts (generate only).
	Vision bool `json:"vision,omitempty"`
	// Reasoning accepts reasoning effort and summary knobs (generate only).
	Reasoning bool `json:"reasoning,omitempty"`
	// Dimensions accepts the embed dimensions knob (embed only).
	Dimensions bool `json:"dimensions,omitempty"`
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
		case kindGenerate, kindEmbed, kindImage, kindTTS, kindASR:
		default:
			return fmt.Errorf(
				"azure: deployment %q has unknown kind %q (want generate|embed|image|tts|asr)",
				model.Name,
				model.Kind,
			)
		}
		if model.Vision && modelKind(model.Kind) != kindGenerate {
			return fmt.Errorf(
				"azure: deployment %q sets vision on kind %q",
				model.Name,
				model.Kind,
			)
		}
		if model.Reasoning && modelKind(model.Kind) != kindGenerate {
			return fmt.Errorf(
				"azure: deployment %q sets reasoning on kind %q",
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
	}
	return nil
}

// isLoopbackHTTP permits plain HTTP only against loopback hosts, which
// keeps test servers and local gateways usable without weakening the
// production https rule.
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

// ProfileSpec carries no profile-scoped settings today: the resource and
// api-version are provider-level.
type ProfileSpec struct{}

func decodeSpec(raw []byte) (Spec, error) {
	var spec Spec
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return Spec{}, fmt.Errorf("azure: provider spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, err
	}
	return spec, nil
}
