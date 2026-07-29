package bytedance

import "fmt"

// modelKind groups models by the service family that serves them. The kind,
// not the model name, selects the compiler and transport.
type modelKind string

const (
	kindGenerate modelKind = "generate"
	kindEmbed    modelKind = "embed"
	kindImage    modelKind = "image"
	kindTTS      modelKind = "tts"
	kindASR      modelKind = "asr"
	kindRealtime modelKind = "realtime"
)

// catalogEntry is one model's declared capability set. Capability flags only
// apply to the kinds they document; other kinds leave them zero.
type catalogEntry struct {
	kind modelKind
	// generate: accepts image input parts.
	vision bool
	// generate: accepts video input parts.
	video bool
	// generate: supports the thinking control behind reasoning effort.
	reasoning bool
	// embed: accepts image items (multimodal embedding).
	imageInput bool
	// embed: accepts custom output dimensions.
	dimensions bool
	// lifecycle: deprecated models stay routable but announce a replacement.
	deprecated  bool
	replacement string
}

// catalog is the built-in model registry. Names are stable Volcengine model
// families; deployment-specific endpoint IDs (ep-xxx or dated revisions like
// doubao-seed-2-1-pro-260628) belong in Spec.Endpoints.
//
// Unknown models fail closed: the factory only exposes models listed here or
// declared via Spec.Models.
var catalog = map[string]catalogEntry{
	// Seed 2.1 (2026-06) — current flagship tier. The whole Seed 2.x line is
	// natively multimodal (text/image/video in) with thinking support.
	"doubao-seed-2-1-pro":   {kind: kindGenerate, vision: true, video: true, reasoning: true},
	"doubao-seed-2-1-turbo": {kind: kindGenerate, vision: true, video: true, reasoning: true},

	// Seed 2.0 (2026-02) — general agent line plus the code-tuned variant.
	"doubao-seed-2-0-pro":  {kind: kindGenerate, vision: true, video: true, reasoning: true},
	"doubao-seed-2-0-lite": {kind: kindGenerate, vision: true, video: true, reasoning: true},
	"doubao-seed-2-0-mini": {kind: kindGenerate, vision: true, video: true, reasoning: true},
	"doubao-seed-2-0-code": {kind: kindGenerate, vision: true, reasoning: true},

	// Seed 1.x — superseded by the 2.x line; kept routable for existing
	// deployments, announced as deprecated with a replacement.
	"doubao-seed-1-8": {
		kind: kindGenerate, vision: true, video: true, reasoning: true,
		deprecated: true, replacement: "doubao-seed-2-0-lite",
	},
	"doubao-seed-1-6": {
		kind: kindGenerate, reasoning: true,
		deprecated: true, replacement: "doubao-seed-2-0-mini",
	},
	"doubao-seed-1-6-vision": {
		kind: kindGenerate, vision: true, reasoning: true,
		deprecated: true, replacement: "doubao-seed-2-0-lite",
	},
	"doubao-seed-1-6-flash": {
		kind: kindGenerate, reasoning: true,
		deprecated: true, replacement: "doubao-seed-2-0-mini",
	},

	"doubao-embedding-large":  {kind: kindEmbed, dimensions: true},
	"doubao-embedding-vision": {kind: kindEmbed, imageInput: true, dimensions: true},

	// Seedream image generation; 5.0/4.5 current, 4.0 superseded.
	"doubao-seedream-5-0": {kind: kindImage},
	"doubao-seedream-4-5": {kind: kindImage},
	"doubao-seedream-4-0": {
		kind: kindImage,
		deprecated: true, replacement: "doubao-seedream-5-0",
	},

	// Logical names for speech families; the wire address (resource ID or
	// fixed upstream model version) is resolved per driver.
	"doubao-tts-2-0":       {kind: kindTTS},
	"doubao-asr-sauc-2-0":  {kind: kindASR},
	"doubao-seeduplex-3-0": {kind: kindRealtime},
}

// mergedCatalog overlays Spec.Models onto the built-in catalog and returns
// the merged view. Spec entries replace catalog entries by name.
func mergedCatalog(spec Spec) (map[string]catalogEntry, error) {
	merged := make(map[string]catalogEntry, len(catalog)+len(spec.Models))
	for name, entry := range catalog {
		merged[name] = entry
	}
	for _, model := range spec.Models {
		merged[model.Name] = catalogEntry{
			kind:       modelKind(model.Kind),
			vision:     model.Vision,
			video:      model.Video,
			reasoning:  model.Reasoning,
			imageInput: model.ImageInput,
			dimensions: model.Dimensions,
		}
	}
	return merged, nil
}

func (e catalogEntry) validate(kind modelKind) error {
	if e.kind != kind {
		return fmt.Errorf("model kind %s does not support %s", e.kind, kind)
	}
	return nil
}
