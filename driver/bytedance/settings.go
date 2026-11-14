package bytedance

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// ProviderID is the identity this driver answers to.
const ProviderID = "bytedance"

// DefaultBaseURL is the Ark API root used when the deployment does not override
// it.
const DefaultBaseURL = "https://ark.cn-beijing.volces.com/api/v3"

type modelKind string

const (
	kindGenerate modelKind = "generate"
	kindImage    modelKind = "image"
	kindVideo    modelKind = "video"
	kindEmbed    modelKind = "embed"
)

// VideoParams declares the Seedance task-parameter support matrix for one
// video model. Zero values mean the deployment did not constrain the parameter.
type VideoParams struct {
	Seed                   bool   `json:"seed,omitempty"`
	CameraFixed            bool   `json:"camera_fixed,omitempty"`
	FlexTier               bool   `json:"flex_tier,omitempty"`
	GenerateAudio          bool   `json:"generate_audio,omitempty"`
	Priority               bool   `json:"priority,omitempty"`
	OutputFormat           bool   `json:"output_format,omitempty"`
	OmniReference          bool   `json:"omni_reference_task_type,omitempty"`
	DurationMin            *int64 `json:"duration_min_seconds,omitempty"`
	DurationMax            *int64 `json:"duration_max_seconds,omitempty"`
	DurationAuto           bool   `json:"duration_auto,omitempty"`
	AudioOnly              bool   `json:"audio_only,omitempty"`
	FrameRatioAdaptiveOnly bool   `json:"frame_ratio_adaptive_only,omitempty"`
	ReferenceImage         int    `json:"reference_image,omitempty"`
	ReferenceVideo         int    `json:"reference_video,omitempty"`
	ReferenceAudio         int    `json:"reference_audio,omitempty"`
}

// Settings is one deployment's configuration. Credentials are already resolved:
// the driver never reads a secret store.
type Settings struct {
	BaseURL    string
	APIKey     string
	Headers    map[string]string
	Query      map[string]string
	Project    string
	Region     string
	Timeout    time.Duration
	Retries    int
	HTTPClient *http.Client

	// VideoPollInterval paces Seedance task polling.
	VideoPollInterval time.Duration
	// ReasoningScope replaces the derived trace scope.
	ReasoningScope string
	// Endpoints maps declared model names to account-scoped Ark endpoint IDs.
	Endpoints map[string]string
	// Models is the deployment's model line-up.
	Models []ModelSpec
}

// ModelSpec declares one model this deployment serves.
type ModelSpec struct {
	Name          string                  `json:"name"`
	Kind          string                  `json:"kind"`
	Capabilities  model.ModelCapabilities `json:"capabilities,omitempty"`
	Limits        model.ModelLimits       `json:"limits,omitempty"`
	Lifecycle     model.ModelLifecycle    `json:"lifecycle,omitzero"`
	MaxResolution string                  `json:"max_resolution,omitempty"`
	Video         VideoParams             `json:"video,omitempty"`
}

// Validate checks one model declaration.
func (m ModelSpec) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("model name is required")
	}
	if err := m.Capabilities.Validate(); err != nil {
		return err
	}
	if err := m.Limits.Validate(); err != nil {
		return err
	}
	switch modelKind(m.Kind) {
	case kindGenerate:
		if !hasOutput(m.Capabilities.Outputs, message.PartText) {
			return fmt.Errorf("generate family must declare text output")
		}
	case kindImage:
		if !hasOutput(m.Capabilities.Outputs, message.PartImage) {
			return fmt.Errorf("image family must declare image output")
		}
	case kindVideo:
		if !hasOutput(m.Capabilities.Outputs, message.PartVideo) {
			return fmt.Errorf("video family must declare video output")
		}
	case kindEmbed:
		if len(m.Capabilities.Outputs) != 0 {
			return fmt.Errorf("embed family declares no generate output")
		}
		if m.Capabilities.EmbeddingDimensions <= 0 {
			return fmt.Errorf(
				"embed family must declare capabilities.embedding_dimensions above zero")
		}
	default:
		return fmt.Errorf("unknown model kind %q", m.Kind)
	}
	return nil
}

func (s Settings) normalized() Settings {
	if strings.TrimSpace(s.BaseURL) == "" {
		s.BaseURL = DefaultBaseURL
	}
	s.BaseURL = strings.TrimRight(s.BaseURL, "/")
	if s.Timeout <= 0 {
		s.Timeout = 10 * time.Minute
	}
	if s.VideoPollInterval <= 0 {
		s.VideoPollInterval = 10 * time.Second
	}
	return s
}

// Validate checks settings that cannot be defaulted.
func (s Settings) Validate() error {
	if strings.TrimSpace(s.APIKey) == "" {
		return fmt.Errorf("bytedance: api key is required")
	}
	if s.Retries < 0 {
		return fmt.Errorf("bytedance: retries must not be negative")
	}
	for _, m := range s.Models {
		if err := m.Validate(); err != nil {
			return fmt.Errorf("bytedance: model %q: %w", m.Name, err)
		}
	}
	return nil
}

func descriptor(spec ModelSpec) model.ModelDescriptor {
	return model.ModelDescriptor{
		ID:           model.ModelID{Provider: ProviderID, Name: spec.Name},
		Capabilities: spec.Capabilities.Clone(),
		Limits:       spec.Limits.Clone(),
		Lifecycle:    spec.Lifecycle.Clone(),
	}
}

func hasOutput(outputs []message.PartKind, want message.PartKind) bool {
	for _, output := range outputs {
		if output == want {
			return true
		}
	}
	return false
}

// Keep context imported for the settings decoder used by callers.
var _ = context.Background
var _ = regexp.MustCompile
