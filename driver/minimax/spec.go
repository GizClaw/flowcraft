package minimax

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

// SecretAPIKey is the provider-owned secret name for the MiniMax API key.
const SecretAPIKey = "api_key"

// Spec is the provider-level configuration surface. It is credential-free:
// secrets resolve per profile and never appear here.
type Spec struct {
	// MediaBaseURL is the media API root (t2a, video, image, music).
	// Defaults to https://api.minimaxi.com; international deployments use
	// https://api.minimax.io. The Messages surface moved to the anthropic
	// driver, which owns its own endpoint.
	MediaBaseURL string `json:"media_base_url,omitempty"`
	// HTTPRetries bounds wire-level HTTP retries on the media client.
	// Zero disables transport retries so the route Router owns the full
	// retry budget; nil keeps the httpkit default. The Anthropic Messages
	// surface rides the vendor SDK and is not governed by this field.
	HTTPRetries *resource.Int `json:"http_retries,omitempty"`
	// VideoPollIntervalMillis paces video task polling; defaults to 5000.
	VideoPollIntervalMillis resource.Int `json:"video_poll_interval_millis,omitempty"`
	// Models declares models outside the built-in catalog or overrides
	// catalog entries by name.
	Models []ModelSpec `json:"models,omitempty"`
}

// ModelSpec declares one model the deployment serves, or a delta over a
// same-named, same-kind built-in catalog entry. Capabilities is a patch
// with field-presence semantics: leaves it names replace that leaf of the
// entry it overrides and leaves it does not name are inherited, so
// redeclaring a built-in to tweak one channel keeps every other declared
// fact.
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
	if m.Kind != "" &&
		modelKind(m.Kind) != kindImage &&
		modelKind(m.Kind) != kindTTS &&
		modelKind(m.Kind) != kindVideo &&
		modelKind(m.Kind) != kindContextIR &&
		modelKind(m.Kind) != kindMusic {
		return fmt.Errorf(
			"model %q declares unsupported kind %q; text generation moved to the anthropic driver (impl: anthropic)",
			m.Name,
			m.Kind,
		)
	}
	if m.Capabilities != nil && m.Capabilities.CustomEmbedDimensions != nil {
		return fmt.Errorf(
			"model %q declares custom_embed_dimensions, but minimax has "+
				"no embed family",
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
	if s.MediaBaseURL != "" {
		if err := validateURL("media_base_url", s.MediaBaseURL); err != nil {
			return err
		}
	}
	if s.VideoPollIntervalMillis < 0 {
		return fmt.Errorf("video_poll_interval_millis must not be negative")
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

// ProfileSpec is the profile-level configuration surface. MiniMax scopes
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
		return Spec{}, fmt.Errorf("minimax spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, fmt.Errorf("minimax spec: %w", err)
	}
	return spec, nil
}

func decodeProfileSpec(ctx context.Context, raw []byte) (ProfileSpec, error) {
	spec, err := resource.DecodeTyped[ProfileSpec](ctx, raw)
	if err != nil {
		return ProfileSpec{}, fmt.Errorf("minimax profile spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return ProfileSpec{}, fmt.Errorf("minimax profile spec: %w", err)
	}
	return spec, nil
}

// mediaBaseURL resolves the media API root: the explicit override, else
// BaseURL with the /anthropic suffix trimmed, else the China default root.
func (s Spec) mediaBaseURL() string {
	if s.MediaBaseURL != "" {
		return s.MediaBaseURL
	}
	return defaultMediaBaseURL
}

// videoPollInterval paces video task polling; the default is 5 seconds.
func (s Spec) videoPollInterval() time.Duration {
	if s.VideoPollIntervalMillis <= 0 {
		return 5 * time.Second
	}
	return time.Duration(s.VideoPollIntervalMillis) * time.Millisecond
}
