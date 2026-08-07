package bytedance

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/inference/config"
)

// Secret names owned by this provider. Profile secrets outside this set are
// rejected at build time so typos fail fast instead of silently missing.
const (
	// SecretAPIKey authenticates the Ark runtime and is the default
	// credential for Doubao speech services.
	SecretAPIKey = "api_key"
	// SecretSpeechAPIKey optionally overrides SecretAPIKey for speech
	// services (TTS, ASR, realtime duplex).
	SecretSpeechAPIKey = "speech_api_key"
	// SecretAccessKey is the Volcengine IAM access key ID. Together with
	// SecretSecretKey it authenticates Ark via AK/SK signing instead of an
	// API key; the pair only covers generate and embed — images, video
	// tasks, and speech services have no AK/SK channel.
	SecretAccessKey = "access_key"
	// SecretSecretKey is the Volcengine IAM secret access key paired with
	// SecretAccessKey.
	SecretSecretKey = "secret_key"
)

// Spec is the provider-level configuration for ByteDance. It must stay
// credential-free: config.DecodeSpec already rejects credential-shaped keys.
type Spec struct {
	// BaseURL overrides the Ark API base URL (regional endpoints, gateways).
	BaseURL string `json:"base_url,omitempty"`
	// SpeechBaseURL overrides the Doubao speech HTTP endpoint (TTS).
	SpeechBaseURL string `json:"speech_base_url,omitempty"`
	// SpeechWebSocketURL overrides the Doubao speech WebSocket endpoint (ASR,
	// realtime duplex).
	SpeechWebSocketURL string `json:"speech_web_socket_url,omitempty"`
	// HTTPRetries bounds wire-level HTTP retries on the Ark and Doubao
	// speech HTTP clients. Zero disables transport retries so the route
	// Router owns the full retry budget; nil keeps the httpkit default.
	HTTPRetries *int `json:"http_retries,omitempty"`
	// Region selects the Ark service region.
	Region string `json:"region,omitempty"`
	// VideoPollIntervalMillis paces content-generation task polls (Seedance
	// video). It tunes client-side waiting only — nothing is sent upstream —
	// so it lives in the deployment Spec, not in a per-request extension.
	// Unset defaults to defaultVideoPollInterval.
	VideoPollIntervalMillis *int64 `json:"video_poll_interval_millis,omitempty"`
	// Models declares additional models beyond the built-in catalog or
	// overrides catalog entries by name.
	Models []ModelSpec `json:"models,omitempty"`
}

// ModelSpec declares one model outside the built-in catalog. Capability
// flags are only meaningful for the matching kind. Addressing a custom model
// at a deployment endpoint works exactly like catalog models: map its name
// in Spec.Endpoints.
type ModelSpec struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Vision (generate) allows image input parts.
	Vision bool `json:"vision,omitempty"`
	// Video (generate) allows video input parts.
	Video bool `json:"video,omitempty"`
	// Reasoning (generate) enables the reasoning effort control.
	Reasoning bool `json:"reasoning,omitempty"`
	// ImageInput (embed) allows image items via multimodal embedding.
	ImageInput bool `json:"image_input,omitempty"`
	// Dimensions (embed) allows custom output dimensions.
	Dimensions bool `json:"dimensions,omitempty"`
	// MaxResolution (video) caps the supported resolution tier, e.g. "720p"
	// or "4k"; empty leaves resolution unconstrained.
	MaxResolution string `json:"max_resolution,omitempty"`
}

// ProfileSpec is the per-credential-profile configuration. Endpoint IDs
// (ep-xxx) and speech app IDs are account-scoped, so both live here rather
// than at provider level: two profiles backed by different Volcengine
// accounts bind the same logical model to different addresses.
type ProfileSpec struct {
	// AppID is the Doubao speech application ID tied to the profile's API
	// key. Speech services (TTS, ASR, realtime duplex) fail to open without
	// it; Ark-only profiles may leave it empty.
	AppID string `json:"app_id,omitempty"`
	// Endpoints maps catalog model names to this account's deployment
	// addresses: Ark inference endpoint IDs (ep-xxx) or speech resource
	// IDs. For realtime models the mapped value pins the duplex dialog
	// engine version instead. Unmapped models are addressed by their
	// catalog name (realtime: the SDK default engine version).
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
	for name, value := range map[string]string{
		"base_url":              s.BaseURL,
		"speech_base_url":       s.SpeechBaseURL,
		"speech_web_socket_url": s.SpeechWebSocketURL,
	} {
		if value == "" {
			continue
		}
		if name == "speech_web_socket_url" {
			if !strings.HasPrefix(value, "wss://") && !strings.HasPrefix(value, "ws://") {
				return fmt.Errorf("speech_web_socket_url must be a ws(s) URL")
			}
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
	case kindGenerate, kindEmbed, kindImage, kindVideo, kindTTS, kindASR, kindRealtime:
	default:
		return fmt.Errorf("model %q has unknown kind %q", m.Name, m.Kind)
	}
	return nil
}

func decodeSpec(raw []byte) (Spec, error) {
	spec, err := config.DecodeSpec[Spec](raw)
	if err != nil {
		return Spec{}, fmt.Errorf("bytedance spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, fmt.Errorf("bytedance spec: %w", err)
	}
	return spec, nil
}

func decodeProfileSpec(raw []byte) (ProfileSpec, error) {
	spec, err := config.DecodeSpec[ProfileSpec](raw)
	if err != nil {
		return ProfileSpec{}, fmt.Errorf("bytedance profile spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return ProfileSpec{}, fmt.Errorf("bytedance profile spec: %w", err)
	}
	return spec, nil
}
