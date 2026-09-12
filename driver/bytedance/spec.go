package bytedance

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/resource"
)

// Secret names owned by this provider. Profile secrets outside this set are
// rejected at build time so typos fail fast instead of silently missing.
const (
	// SecretAPIKey authenticates the Ark runtime.
	SecretAPIKey = "api_key"
)

// Spec is the provider-level configuration for ByteDance. It must stay
// credential-free: config.DecodeSpec already rejects credential-shaped keys.
type Spec struct {
	// BaseURL overrides the Ark API base URL (regional endpoints, gateways).
	BaseURL string `json:"base_url,omitempty"`
	// HTTPRetries bounds wire-level HTTP retries on the Ark HTTP client.
	// Zero disables transport retries so the route Router owns the full
	// retry budget; nil keeps the httpkit default.
	HTTPRetries *resource.Int `json:"http_retries,omitempty"`
	// Region selects the Ark service region.
	Region string `json:"region,omitempty"`
	// Project sets the Ark project name requests are attributed to.
	Project string `json:"project,omitempty"`
	// Timeout bounds one HTTP request to Ark, as a Go duration string
	// ("90s", "2m"). Empty keeps the driver default. It bounds a single
	// attempt, not the whole logical inference call, which the route Router
	// owns.
	Timeout string `json:"timeout,omitempty"`
	// Headers adds static headers to every request. Use this for gateway
	// routing hints; credentials belong in the profile's api_key secret.
	Headers map[string]string `json:"headers,omitempty"`
	// Query adds query parameters to every request.
	Query map[string]string `json:"query,omitempty"`
	// VideoPollIntervalMillis paces content-generation task polls (Seedance
	// video). It tunes client-side waiting only — nothing is sent upstream —
	// so it lives in the deployment Spec, not in a per-request extension.
	// Unset defaults to defaultVideoPollInterval.
	VideoPollIntervalMillis *resource.Int64 `json:"video_poll_interval_millis,omitempty"`
	// ReasoningScope declares the verification scope this deployment's
	// reasoning traces belong to. Ark emits traces but consumes none today, so
	// the scope is what makes a trace attributable when a conversation moves
	// to a target that would replay one: it replaces the derived scope —
	// provider, model, and credential profile — and unset keeps that
	// conservative default.
	ReasoningScope string `json:"reasoning_scope,omitempty"`
	// Models declares additional models beyond the built-in catalog or
	// overrides catalog entries by name.
	Models []ModelSpec `json:"models,omitempty"`
}

// ModelSpec declares one model outside the built-in catalog, or a delta over
// a same-named, same-kind built-in catalog entry. Capabilities is a patch
// with field-presence semantics: leaves it names replace that leaf of the
// entry it overrides and leaves it does not name are inherited. Custom
// embed output dimensions live in capabilities.custom_embed_dimensions;
// max_resolution is a control fact that no capability kind expresses and
// stays a separate flag — nil inherits the overridden built-in value, an
// explicit empty string declares resolution unconstrained. Addressing a
// custom model at a deployment endpoint works exactly like catalog models:
// map its name in Spec.Endpoints.
type ModelSpec struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Capabilities declares the capability leaves this model changes.
	Capabilities *model.CapabilitiesPatch `json:"capabilities,omitempty"`
	// Limits declares numeric capacity limits for the model. Overriding a
	// built-in catalog entry by name keeps the catalog limit for any field
	// left nil; declaring a value replaces it.
	Limits model.ModelLimits `json:"limits,omitempty"`
	// MaxResolution (video) caps the supported resolution tier, e.g. "720p"
	// or "4k". Nil inherits the built-in value; an explicit empty string
	// declares resolution unconstrained.
	MaxResolution *string `json:"max_resolution,omitempty"`
}

// ProfileSpec is the per-credential-profile configuration. Endpoint IDs
// (ep-xxx) are account-scoped, so they live here rather than at provider
// level: two profiles backed by different Volcengine accounts bind the same
// logical model to different addresses.
type ProfileSpec struct {
	// Endpoints maps catalog model names to this account's deployment
	// addresses: Ark inference endpoint IDs (ep-xxx). Unmapped models are
	// addressed by their catalog name.
	Endpoints map[string]string `json:"endpoints,omitempty"`
}

func (s ProfileSpec) Validate() error {
	for name, endpoint := range s.Endpoints {
		if !modelNamePattern.MatchString(name) {
			return fmt.Errorf("endpoints: invalid model name %q", name)
		}
		if strings.TrimSpace(endpoint) == "" {
			return fmt.Errorf("endpoints[%q] is empty", name)
		}
	}
	return nil
}

var modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// videoPollInterval resolves the task poll pacing for Seedance transports.
func (s Spec) videoPollInterval() time.Duration {
	if s.VideoPollIntervalMillis != nil {
		return time.Duration(*s.VideoPollIntervalMillis) * time.Millisecond
	}
	return defaultVideoPollInterval
}

func (s Spec) Validate() error {
	if s.VideoPollIntervalMillis != nil && *s.VideoPollIntervalMillis <= 0 {
		return fmt.Errorf("video_poll_interval_millis must be positive")
	}
	if s.HTTPRetries != nil && *s.HTTPRetries < 0 {
		return fmt.Errorf("http_retries must not be negative")
	}
	if err := validateReasoningScope(s.ReasoningScope); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"base_url": s.BaseURL,
	} {
		if value == "" {
			continue
		}
		if !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "http://") {
			return fmt.Errorf("%s must be an http(s) URL", name)
		}
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
	case kindGenerate, kindEmbed, kindImage, kindVideo:
	default:
		return fmt.Errorf("model %q has unknown kind %q", m.Name, m.Kind)
	}
	kind := modelKind(m.Kind)
	if m.Capabilities != nil &&
		m.Capabilities.CustomEmbedDimensions != nil &&
		*m.Capabilities.CustomEmbedDimensions &&
		kind != kindEmbed {
		return fmt.Errorf(
			"model %q sets custom_embed_dimensions on kind %q",
			m.Name,
			m.Kind,
		)
	}
	if m.MaxResolution != nil && kind != kindVideo {
		return fmt.Errorf("model %q sets max_resolution on kind %q", m.Name, m.Kind)
	}
	if err := m.Capabilities.Validate(); err != nil {
		return err
	}
	return m.Limits.Validate()
}

func decodeSpec(ctx context.Context, raw []byte) (Spec, error) {
	spec, err := resource.DecodeTyped[Spec](ctx, raw)
	if err != nil {
		return Spec{}, fmt.Errorf("bytedance spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, fmt.Errorf("bytedance spec: %w", err)
	}
	return spec, nil
}

func decodeProfileSpec(ctx context.Context, raw []byte) (ProfileSpec, error) {
	spec, err := resource.DecodeTyped[ProfileSpec](ctx, raw)
	if err != nil {
		return ProfileSpec{}, fmt.Errorf("bytedance profile spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return ProfileSpec{}, fmt.Errorf("bytedance profile spec: %w", err)
	}
	return spec, nil
}

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
			"reasoning_scope must not have surrounding whitespace")
	}
	if len(scope) > maxReasoningScopeLen {
		return fmt.Errorf(
			"reasoning_scope is %d bytes, at most %d are allowed",
			len(scope), maxReasoningScopeLen,
		)
	}
	for _, char := range scope {
		if unicode.IsControl(char) {
			return fmt.Errorf(
				"reasoning_scope must not contain control characters")
		}
	}
	return nil
}
