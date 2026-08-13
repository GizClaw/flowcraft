package bytedance

import "maps"

// modelKind groups models by the service family that serves them. The kind,
// not the model name, selects the compiler and transport.
type modelKind string

const (
	kindGenerate modelKind = "generate"
	kindEmbed    modelKind = "embed"
	kindImage    modelKind = "image"
	kindVideo    modelKind = "video"
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
	// webSearch (generate) accepts the hosted Web Search (联网内容插件)
	// tool. Support is per model per the Ark tool-calling capability
	// matrix; see
	// https://www.volcengine.com/docs/82379/1330310#f44ceef7.
	webSearch bool
	// embed: accepts image items (multimodal embedding).
	imageInput bool
	// embed: accepts custom output dimensions.
	dimensions bool
	// video: highest supported resolution tier ("720p", "1080p", "4k");
	// empty leaves resolution unconstrained.
	maxResolution string
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
	"doubao-seed-2-1-pro":   {kind: kindGenerate, vision: true, video: true, reasoning: true, webSearch: true},
	"doubao-seed-2-1-turbo": {kind: kindGenerate, vision: true, video: true, reasoning: true, webSearch: true},

	// Seed 2.0 (2026-02) — general agent line plus the code-tuned variant.
	"doubao-seed-2-0-pro":  {kind: kindGenerate, vision: true, video: true, reasoning: true, webSearch: true},
	"doubao-seed-2-0-lite": {kind: kindGenerate, vision: true, video: true, reasoning: true, webSearch: true},
	"doubao-seed-2-0-mini": {kind: kindGenerate, vision: true, video: true, reasoning: true, webSearch: true},
	"doubao-seed-2-0-code": {kind: kindGenerate, vision: true, reasoning: true, webSearch: true},

	// Seed 1.x — superseded by the 2.x line; kept routable for existing
	// deployments, announced as deprecated with a replacement.
	"doubao-seed-1-8": {
		kind: kindGenerate, vision: true, video: true, reasoning: true, webSearch: true,
		deprecated: true, replacement: "doubao-seed-2-0-lite",
	},
	"doubao-seed-1-6": {
		kind: kindGenerate, reasoning: true, webSearch: true,
		deprecated: true, replacement: "doubao-seed-2-0-mini",
	},
	"doubao-seed-1-6-vision": {
		kind: kindGenerate, vision: true, reasoning: true, webSearch: true,
		deprecated: true, replacement: "doubao-seed-2-0-lite",
	},
	"doubao-seed-1-6-flash": {
		kind: kindGenerate, reasoning: true, webSearch: true,
		deprecated: true, replacement: "doubao-seed-2-0-mini",
	},

	"doubao-embedding-large":  {kind: kindEmbed, dimensions: true},
	"doubao-embedding-vision": {kind: kindEmbed, imageInput: true, dimensions: true},

	// Seedream image generation; 5.0/4.5 current, 4.0 superseded.
	"doubao-seedream-5-0": {kind: kindImage},
	"doubao-seedream-4-5": {kind: kindImage},
	"doubao-seedream-4-0": {
		kind:       kindImage,
		deprecated: true, replacement: "doubao-seedream-5-0",
	},

	// Seedance video generation, served by the async content-generation task
	// API behind the unary contract. 2.0 is the current line; fast/mini cap
	// at 720p. 1.x stays routable for existing deployments.
	"doubao-seedance-2-0":      {kind: kindVideo, maxResolution: "4k"},
	"doubao-seedance-2-0-fast": {kind: kindVideo, maxResolution: "720p"},
	"doubao-seedance-2-0-mini": {kind: kindVideo, maxResolution: "720p"},
	"doubao-seedance-1-5-pro": {
		kind: kindVideo, maxResolution: "1080p",
		deprecated: true, replacement: "doubao-seedance-2-0",
	},
	"doubao-seedance-1-0-pro": {
		kind: kindVideo, maxResolution: "1080p",
		deprecated: true, replacement: "doubao-seedance-2-0",
	},
	"doubao-seedance-1-0-lite-t2v": {
		kind: kindVideo, maxResolution: "720p",
		deprecated: true, replacement: "doubao-seedance-2-0-fast",
	},
	"doubao-seedance-1-0-lite-i2v": {
		kind: kindVideo, maxResolution: "720p",
		deprecated: true, replacement: "doubao-seedance-2-0-fast",
	},

	// Logical names for speech families; the wire address (resource ID or
	// fixed upstream model version) is resolved per driver. ASR and realtime
	// families are absent until core exposes those operation surfaces.
	"doubao-tts-2-0": {kind: kindTTS},
}

// mergedCatalog overlays Spec.Models onto the built-in catalog and returns
// the merged view. Spec entries replace catalog entries by name.
func mergedCatalog(spec Spec) (map[string]catalogEntry, error) {
	merged := make(map[string]catalogEntry, len(catalog)+len(spec.Models))
	maps.Copy(merged, catalog)
	for _, model := range spec.Models {
		merged[model.Name] = catalogEntry{
			kind:          modelKind(model.Kind),
			vision:        model.Vision,
			video:         model.Video,
			reasoning:     model.Reasoning,
			webSearch:     model.WebSearch,
			imageInput:    model.ImageInput,
			dimensions:    model.Dimensions,
			maxResolution: model.MaxResolution,
		}
	}
	return merged, nil
}
