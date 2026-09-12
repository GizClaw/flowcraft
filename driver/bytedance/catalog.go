package bytedance

import (
	"fmt"
	"maps"
	"slices"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// modelKind groups models by the service family that serves them. The kind,
// not the model name, selects the compiler and transport.
type modelKind string

const (
	kindGenerate modelKind = "generate"
	kindEmbed    modelKind = "embed"
	kindImage    modelKind = "image"
	kindVideo    modelKind = "video"
)

// catalogEntry is one model's declared capability set. capabilities is the
// single capability fact source: input/output content kinds, hosted web
// search, the reasoning control capability, and custom embed output
// dimensions. maxResolution is a control capability that no capability kind
// expresses and stays a separate flag.
type catalogEntry struct {
	kind         modelKind
	capabilities model.ModelCapabilities
	// limits carries the model's context/output windows in tokens. Nil
	// leaves are undeclared. Generate values mirror the family context
	// window and per-version max output on the Volcengine Ark model detail
	// pages (https://www.volcengine.com/docs/82379/1330310); embedding
	// values mirror the documented per-input limit.
	limits model.ModelLimits
	// video: highest supported resolution tier ("720p", "1080p", "4k");
	// empty leaves resolution unconstrained.
	maxResolution string
	// video: Seedance task-parameter support matrix (see videoParams).
	video videoParams
	// lifecycle: deprecated models stay routable but announce a replacement.
	deprecated  bool
	replacement string
}

// videoParams is the Seedance task-parameter support matrix for one video
// model, transcribed from the official create-task API documentation
// (https://www.volcengine.com/docs/82379/1520757): each field mirrors one
// parameter's documented "model support" column, so the compiler can reject
// parameters the strong-validation endpoint would otherwise fault on.
// Zero values mean "undeclared": custom Spec.Models entries (names outside
// the built-in catalog) keep a zero matrix and compile with syntax-only
// validation (the deployment declares the capability); redeclarations of
// built-in video models inherit the built-in matrix, exactly like every
// other control flag.
type videoParams struct {
	seed          bool // supports seed
	cameraFixed   bool // supports camera_fixed
	flexTier      bool // supports service_tier=flex
	generateAudio bool // supports generate_audio
	priority      bool // supports priority
	outputFormat  bool // supports output_format
	omniReference bool // supports omni_reference_task_type
	durationMin   *int64
	durationMax   *int64
	durationAuto  bool // supports duration=-1 (model picks the length)
	// audioOnly allows audio-only input (2.5); other reference-capable
	// models require at least one image or video alongside audio.
	audioOnly bool
	// frameRatioAdaptiveOnly restricts first/last-frame tasks to
	// ratio=adaptive (2.5); explicit ratios are rejected for them.
	frameRatioAdaptiveOnly bool
	// Reference-input caps; 0 means the model takes no reference inputs
	// (image counts above the first/last-frame pair are rejected).
	referenceImage int
	referenceVideo int
	referenceAudio int
}

// videoSeconds builds a *int64 for videoParams duration bounds.
func videoSeconds(value int64) *int64 { return &value }

// validate enforces the family contract: the compiler bound by kind can only
// serve the output modalities it produces, so kind and capabilities cannot
// drift.
func (e catalogEntry) validate() error {
	if err := e.capabilities.Validate(); err != nil {
		return err
	}
	switch e.kind {
	case kindGenerate:
		if !slices.Contains(e.capabilities.Outputs, message.PartText) {
			return fmt.Errorf("generate family must declare text output")
		}
	case kindImage:
		if !slices.Contains(e.capabilities.Outputs, message.PartImage) {
			return fmt.Errorf("image family must declare image output")
		}
	case kindVideo:
		if !slices.Contains(e.capabilities.Outputs, message.PartVideo) {
			return fmt.Errorf("video family must declare video output")
		}
		if e.video.referenceImage > 0 && !slices.Contains(e.capabilities.Inputs, message.PartImage) {
			return fmt.Errorf("video family declaring reference images must accept image input")
		}
		if e.video.referenceVideo > 0 && !slices.Contains(e.capabilities.Inputs, message.PartVideo) {
			return fmt.Errorf("video family declaring reference videos must accept video input")
		}
		if e.video.referenceAudio > 0 && !slices.Contains(e.capabilities.Inputs, message.PartAudio) {
			return fmt.Errorf("video family declaring reference audio must accept audio input")
		}
	case kindEmbed:
		if len(e.capabilities.Outputs) != 0 {
			return fmt.Errorf("%s family declares no generate output", e.kind)
		}
	}
	return e.limits.Validate()
}

// generateChatCapabilities is the common capability declaration for the Ark
// text compiler family. Individual entries add image/video input, hosted web
// search, and the reasoning control capability. Ark consumes no reasoning
// input, so PartReasoning is deliberately absent.
func generateChatCapabilities() model.ModelCapabilities {
	return model.ModelCapabilities{
		Inputs: []message.PartKind{
			message.PartText,
			message.PartData,
			message.PartToolCall,
			message.PartToolResult,
		},
		Outputs: []message.PartKind{message.PartText},
	}
}

// arkEffortMap is the canonical-to-wire map shared by the Ark thinking
// models. The SDK's reasoning_effort enum carries low/medium/high only, so
// canonical minimal folds onto low and xhigh folds onto high; Doubao's
// documented minimal level is its no-thinking mode, which the canonical
// ReasoningEnabled switch covers instead.
var arkEffortMap = map[model.ReasoningEffort]string{
	model.ReasoningMinimal: string(model.ReasoningLow),
	model.ReasoningLow:     string(model.ReasoningLow),
	model.ReasoningMedium:  string(model.ReasoningMedium),
	model.ReasoningHigh:    string(model.ReasoningHigh),
	model.ReasoningXHigh:   string(model.ReasoningHigh),
}

// catalog is the built-in model registry. Names are stable Volcengine model
// families; deployment-specific endpoint IDs (ep-xxx or dated revisions like
// doubao-seed-2-1-pro-260628) belong in Spec.Endpoints.
//
// Unknown models fail closed: the factory only exposes models listed here or
// declared via Spec.Models.
var catalog = map[string]catalogEntry{
	// Doubao Seed Evolving (2026-07) — rapidly iterating Coding & Agent
	// line, updated weekly. Long 1M context; natively multimodal with
	// thinking and the full hosted tool surface.
	"doubao-seed-evolving": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithHostedWebSearch().
			WithReasoning(model.ReasoningToggle).
			WithReasoningEffortMap(arkEffortMap),
		limits: model.ModelLimits{}.
			WithMaxInputTokens(1_024_000).
			WithMaxOutputTokens(256_000),
	},

	// Seed 2.1 (2026-06) — current flagship tier. The whole Seed 2.x line is
	// natively multimodal (text/image/video in) with thinking support.
	"doubao-seed-2-1-pro": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithHostedWebSearch().
			WithReasoning(model.ReasoningToggle).
			WithReasoningEffortMap(arkEffortMap),
		limits: model.ModelLimits{}.
			WithMaxInputTokens(256_000).
			WithMaxOutputTokens(256_000),
	},
	"doubao-seed-2-1-turbo": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithHostedWebSearch().
			WithReasoning(model.ReasoningToggle).
			WithReasoningEffortMap(arkEffortMap),
		limits: model.ModelLimits{}.
			WithMaxInputTokens(256_000).
			WithMaxOutputTokens(256_000),
	},

	// Seed 2.0 (2026-02) — general agent line plus the code-tuned variant.
	// The 260215 revisions retired in 2026-05, but the 260428 lite/mini
	// revisions stay live and are Ark's recommended audio-understanding
	// models, so the stable family names remain routable.
	"doubao-seed-2-0-pro": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithHostedWebSearch().
			WithReasoning(model.ReasoningToggle).
			WithReasoningEffortMap(arkEffortMap),
		limits: model.ModelLimits{}.
			WithMaxInputTokens(256_000).
			WithMaxOutputTokens(128_000),
	},
	"doubao-seed-2-0-lite": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo, message.PartAudio).
			WithHostedWebSearch().
			WithReasoning(model.ReasoningToggle).
			WithReasoningEffortMap(arkEffortMap),
		limits: model.ModelLimits{}.
			WithMaxInputTokens(256_000).
			WithMaxOutputTokens(128_000),
	},
	"doubao-seed-2-0-mini": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo, message.PartAudio).
			WithHostedWebSearch().
			WithReasoning(model.ReasoningToggle).
			WithReasoningEffortMap(arkEffortMap),
		limits: model.ModelLimits{}.
			WithMaxInputTokens(256_000).
			WithMaxOutputTokens(128_000),
	},
	"doubao-seed-2-0-code": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithHostedWebSearch().
			WithReasoning(model.ReasoningToggle).
			WithReasoningEffortMap(arkEffortMap),
		limits: model.ModelLimits{}.
			WithMaxInputTokens(256_000).
			WithMaxOutputTokens(128_000),
	},

	// Seed 1.x — superseded by the 2.x line; kept routable for existing
	// deployments, announced as deprecated with a replacement.
	"doubao-seed-1-8": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage, message.PartVideo).
			WithHostedWebSearch().
			WithReasoning(model.ReasoningToggle).
			WithReasoningEffortMap(arkEffortMap),
		deprecated: true, replacement: "doubao-seed-2-0-lite",
		limits: model.ModelLimits{}.
			WithMaxInputTokens(256_000).
			WithMaxOutputTokens(64_000),
	},
	"doubao-seed-1-6-vision": {
		kind: kindGenerate,
		capabilities: generateChatCapabilities().
			WithInputs(message.PartImage).
			WithHostedWebSearch().
			WithReasoning(model.ReasoningToggle).
			WithReasoningEffortMap(arkEffortMap),
		deprecated: true, replacement: "doubao-seed-2-0-lite",
		limits: model.ModelLimits{}.
			WithMaxInputTokens(256_000).
			WithMaxOutputTokens(64_000),
	},

	"doubao-embedding-large": {
		kind: kindEmbed,
		capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText).
			WithCustomEmbedDimensions(),
		limits: model.ModelLimits{}.WithMaxInputTokens(4_095),
	},
	"doubao-embedding-vision": {
		kind: kindEmbed,
		capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithCustomEmbedDimensions(),
		limits: model.ModelLimits{}.WithMaxInputTokens(8_191),
	},

	// Seedream image generation; 5.0-pro/5.0/4.5 current, 4.0 superseded.
	"doubao-seedream-5-0-pro": {
		kind: kindImage,
		capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartImage),
	},
	"doubao-seedream-5-0": {
		kind: kindImage,
		capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartImage),
	},
	"doubao-seedream-4-5": {
		kind: kindImage,
		capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartImage),
	},
	"doubao-seedream-4-0": {
		kind: kindImage,
		capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartImage),
		deprecated: true, replacement: "doubao-seedream-5-0",
	},

	// Seedance video generation, served by the async content-generation task
	// API behind the unary contract. 2.0 is the current line; fast/mini cap
	// at 720p. 2.5 is the current flagship (1080p, 30s narrative). 1.x
	// stays routable for existing deployments.
	"doubao-seedance-2-5": {
		kind: kindVideo,
		capabilities: model.ModelCapabilities{}.
			WithInputs(
				message.PartText,
				message.PartImage,
				message.PartVideo,
				message.PartAudio,
			).
			WithOutputs(message.PartVideo).
			WithHostedWebSearch(),
		maxResolution: "1080p",
		video: videoParams{
			generateAudio:          true,
			priority:               true,
			outputFormat:           true,
			omniReference:          true,
			durationMin:            videoSeconds(4),
			durationMax:            videoSeconds(30),
			durationAuto:           true,
			audioOnly:              true,
			frameRatioAdaptiveOnly: true,
			referenceImage:         30,
			referenceVideo:         10,
			referenceAudio:         10,
		},
	},
	"doubao-seedance-2-0": {
		kind: kindVideo,
		capabilities: model.ModelCapabilities{}.
			WithInputs(
				message.PartText,
				message.PartImage,
				message.PartVideo,
				message.PartAudio,
			).
			WithOutputs(message.PartVideo).
			WithHostedWebSearch(),
		maxResolution: "4k",
		video: videoParams{
			generateAudio:  true,
			priority:       true,
			durationMin:    videoSeconds(4),
			durationMax:    videoSeconds(15),
			durationAuto:   true,
			referenceImage: 9,
			referenceVideo: 3,
			referenceAudio: 3,
		},
	},
	"doubao-seedance-2-0-fast": {
		kind: kindVideo,
		capabilities: model.ModelCapabilities{}.
			WithInputs(
				message.PartText,
				message.PartImage,
				message.PartVideo,
				message.PartAudio,
			).
			WithOutputs(message.PartVideo).
			WithHostedWebSearch(),
		maxResolution: "720p",
		video: videoParams{
			generateAudio:  true,
			priority:       true,
			durationMin:    videoSeconds(4),
			durationMax:    videoSeconds(15),
			durationAuto:   true,
			referenceImage: 9,
			referenceVideo: 3,
			referenceAudio: 3,
		},
	},
	"doubao-seedance-2-0-mini": {
		kind: kindVideo,
		capabilities: model.ModelCapabilities{}.
			WithInputs(
				message.PartText,
				message.PartImage,
				message.PartVideo,
				message.PartAudio,
			).
			WithOutputs(message.PartVideo).
			WithHostedWebSearch(),
		maxResolution: "720p",
		video: videoParams{
			generateAudio:  true,
			priority:       true,
			durationMin:    videoSeconds(4),
			durationMax:    videoSeconds(15),
			durationAuto:   true,
			referenceImage: 9,
			referenceVideo: 3,
			referenceAudio: 3,
		},
	},
	"doubao-seedance-1-5-pro": {
		kind: kindVideo,
		capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		maxResolution: "1080p",
		video: videoParams{
			seed:          true,
			cameraFixed:   true,
			flexTier:      true,
			generateAudio: true,
			durationMin:   videoSeconds(4),
			durationMax:   videoSeconds(12),
			durationAuto:  true,
		},
		deprecated: true, replacement: "doubao-seedance-2-0",
	},
	"doubao-seedance-1-0-pro": {
		kind: kindVideo,
		capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		maxResolution: "1080p",
		video: videoParams{
			seed:        true,
			cameraFixed: true,
			flexTier:    true,
			durationMin: videoSeconds(2),
			durationMax: videoSeconds(12),
		},
		deprecated: true, replacement: "doubao-seedance-2-0",
	},
	"doubao-seedance-1-0-lite-t2v": {
		kind: kindVideo,
		capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		maxResolution: "720p",
		// Official docs list only the 1.0 pro/pro fast tiers; the lite
		// entries approximate the 1.0 family support set.
		video: videoParams{
			seed:        true,
			cameraFixed: true,
			flexTier:    true,
			durationMin: videoSeconds(2),
			durationMax: videoSeconds(12),
		},
		deprecated: true, replacement: "doubao-seedance-2-0-fast",
	},
	"doubao-seedance-1-0-lite-i2v": {
		kind: kindVideo,
		capabilities: model.ModelCapabilities{}.
			WithInputs(message.PartText, message.PartImage).
			WithOutputs(message.PartVideo),
		maxResolution: "720p",
		video: videoParams{
			seed:        true,
			cameraFixed: true,
			flexTier:    true,
			durationMin: videoSeconds(2),
			durationMax: videoSeconds(12),
		},
		deprecated: true, replacement: "doubao-seedance-2-0-fast",
	},
}

// mergedCatalog overlays Spec.Models onto the built-in catalog and returns
// the merged view. A spec entry with a built-in name under the same kind is
// a leaf-level patch: capability leaves it names replace the built-in's and
// everything else (numeric limits, max resolution, the video parameter
// matrix) is inherited unless declared explicitly. Video entries
// intentionally keep a zero parameter matrix (syntax-only validation) when
// the deployment declares its own capability set; see videoParams.
func mergedCatalog(spec Spec) (map[string]catalogEntry, error) {
	merged := make(map[string]catalogEntry, len(catalog)+len(spec.Models))
	maps.Copy(merged, catalog)
	for _, declared := range spec.Models {
		kind := modelKind(declared.Kind)
		builtin, exists := merged[declared.Name]
		entry := catalogEntry{kind: kind}
		if exists && builtin.kind == kind {
			entry.capabilities = declared.Capabilities.Apply(builtin.capabilities)
			entry.limits = builtin.limits.Clone()
			entry.video = builtin.video
			entry.maxResolution = builtin.maxResolution
			if declared.MaxResolution != nil {
				entry.maxResolution = *declared.MaxResolution
			}
		} else {
			entry.capabilities = declared.Capabilities.Apply(
				model.ModelCapabilities{},
			)
			if declared.MaxResolution != nil {
				entry.maxResolution = *declared.MaxResolution
			}
		}
		if declared.Limits.MaxInputTokens != nil {
			value := *declared.Limits.MaxInputTokens
			entry.limits.MaxInputTokens = &value
		}
		if declared.Limits.MaxOutputTokens != nil {
			value := *declared.Limits.MaxOutputTokens
			entry.limits.MaxOutputTokens = &value
		}
		merged[declared.Name] = entry
	}
	for name, entry := range merged {
		if err := entry.validate(); err != nil {
			return nil, fmt.Errorf("catalog model %q: %w", name, err)
		}
	}
	return merged, nil
}

// descriptorFor lowers one catalog entry into its public discovery
// descriptor under id. buildProvider and Catalog share this lowering so
// offline catalog views cannot drift from deployed provider models.
func descriptorFor(id model.ModelID, entry catalogEntry) model.ModelDescriptor {
	descriptor := model.ModelDescriptor{
		ID:           id,
		Capabilities: entry.capabilities,
	}
	if entry.deprecated {
		descriptor.Lifecycle.Status = model.ModelStatusDeprecated
		if entry.replacement != "" {
			replacement := model.ModelID{
				Provider: id.Provider,
				Name:     entry.replacement,
			}
			descriptor.Lifecycle.Replacement = &replacement
		}
	}
	descriptor.Limits = entry.limits.Clone()
	return descriptor
}

// Catalog returns every built-in model descriptor under provider, sorted by
// model name. Descriptors carry the driver-declared capabilities, limits, and
// lifecycle; Operations stay empty because the inference assembly derives them
// from the model's openers after deployment. Provider IDs are a deployment
// property, so callers supply the identity that appears in each descriptor.
func Catalog(provider string) ([]model.ModelDescriptor, error) {
	if provider == "" {
		return nil, fmt.Errorf("catalog: provider is required")
	}
	names := make([]string, 0, len(catalog))
	for name := range catalog {
		names = append(names, name)
	}
	slices.Sort(names)
	descriptors := make([]model.ModelDescriptor, 0, len(catalog))
	for _, name := range names {
		descriptor := descriptorFor(
			model.ModelID{Provider: provider, Name: name},
			catalog[name],
		)
		descriptors = append(descriptors, descriptor)
	}
	return descriptors, nil
}
