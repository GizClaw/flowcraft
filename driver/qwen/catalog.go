package qwen

import (
	"fmt"
)

type modelKind string

const (
	kindGenerate modelKind = "generate"
	kindEmbed    modelKind = "embed"
)

// catalogEntry declares what one catalog model accepts. The zero value is
// the bare text surface: the compiler rejects every channel the entry
// omits, so a model never silently accepts a feature it may not serve.
type catalogEntry struct {
	kind modelKind
	// vision accepts image input parts; multimodal models ride the
	// multimodal-generation endpoint rather than text-generation, and the
	// multimodal-embedding endpoint rather than text-embedding.
	vision bool
	// video accepts video input parts (frame lists or files) on the
	// multimodal endpoint.
	video bool
	// reasoning accepts the thinking switch (enable_thinking) and emits
	// reasoning_content traces.
	reasoning bool
	// reasoningEffort accepts the reasoning_effort levels
	// (qwen3.8-max-preview only); other thinking models take
	// thinking_budget through the extension instead.
	reasoningEffort bool
	// preserveThinking can re-ingest reasoning_content history
	// (preserve_thinking); models without it drop round-trip traces.
	preserveThinking bool
	// thinkingStreamOnly marks models whose thinking mode answers on SSE
	// only: a unary compile with thinking on rejects the unary shape.
	thinkingStreamOnly bool
	// embedDimensions lists the vector sizes an embed model accepts; nil
	// means the model takes no dimension parameter.
	embedDimensions []int
}

// catalog reflects the DashScope commercial lineup as of 2026-07.
// Sources:
//   - https://www.alibabacloud.com/help/zh/model-studio/qwen-api-reference
//   - https://www.alibabacloud.com/help/zh/model-studio/models
//
// The qwen3.7/qwen3.8 commercial models are multimodal and hybrid-thinking;
// thinking mode is stream-only server-side, so unary compiles with
// thinking on reject the shape. The legacy qwen-plus/turbo/flash/max
// names stay text-only here — custom models declare through the spec.
var catalog = map[string]catalogEntry{
	"qwen3.8-max-preview": {kind: kindGenerate, vision: true, video: true, reasoning: true, reasoningEffort: true, preserveThinking: true, thinkingStreamOnly: true},
	"qwen3.7-max":         {kind: kindGenerate, vision: true, video: true, reasoning: true, preserveThinking: true, thinkingStreamOnly: true},
	"qwen3.7-plus":        {kind: kindGenerate, vision: true, video: true, reasoning: true, preserveThinking: true, thinkingStreamOnly: true},
	"qwen3.7-flash":       {kind: kindGenerate, vision: true, video: true, reasoning: true, preserveThinking: true, thinkingStreamOnly: true},
	"qwen3-vl-plus":       {kind: kindGenerate, vision: true, video: true, reasoning: true, thinkingStreamOnly: true},
	"qwen3-vl-flash":      {kind: kindGenerate, vision: true, video: true, reasoning: true, thinkingStreamOnly: true},

	"qwen-plus":  {kind: kindGenerate},
	"qwen-turbo": {kind: kindGenerate},
	"qwen-flash": {kind: kindGenerate},
	"qwen-max":   {kind: kindGenerate},

	// Embeddings. The multimodal model is served in the Beijing region
	// only; text-embedding-v4 batches at most 10 rows per request.
	"text-embedding-v4": {
		kind:            kindEmbed,
		embedDimensions: []int{2048, 1536, 1024, 768, 512, 256, 128, 64},
	},
	"qwen3-vl-embedding": {
		kind:            kindEmbed,
		vision:          true,
		video:           true,
		embedDimensions: []int{2560, 2048, 1536, 1024, 768, 512, 256},
	},
}

func (e catalogEntry) validate() error {
	if e.kind != kindGenerate && e.kind != kindEmbed {
		return fmt.Errorf("unsupported kind %q", e.kind)
	}
	if e.kind == kindEmbed && (e.reasoning || e.reasoningEffort || e.preserveThinking || e.thinkingStreamOnly) {
		return fmt.Errorf("embed model cannot declare thinking flags")
	}
	return nil
}

// multimodal reports whether the model rides the multimodal-generation
// endpoint rather than text-generation.
func (e catalogEntry) multimodal() bool { return e.vision || e.video }

// mergedCatalog overlays the spec's declared models on the built-in
// catalog. Unknown kinds are rejected at build time; custom models get the
// bare declared surface (fail closed).
func mergedCatalog(spec Spec) (map[string]catalogEntry, error) {
	merged := make(map[string]catalogEntry, len(catalog)+len(spec.Models))
	for name, entry := range catalog {
		if err := entry.validate(); err != nil {
			return nil, fmt.Errorf("catalog model %q: %w", name, err)
		}
		merged[name] = entry
	}
	for _, model := range spec.Models {
		if entry, exists := merged[model.Name]; exists {
			if model.Kind != "" && modelKind(model.Kind) != entry.kind {
				return nil, fmt.Errorf("model %q kind %q conflicts with catalog %q",
					model.Name, model.Kind, entry.kind)
			}
		}
		entry := merged[model.Name]
		if entry.kind == "" {
			entry.kind = modelKind(model.Kind)
			if entry.kind == "" {
				entry.kind = kindGenerate
			}
		}
		entry.vision = entry.vision || model.Vision
		entry.reasoning = entry.reasoning || model.Reasoning
		if err := entry.validate(); err != nil {
			return nil, fmt.Errorf("model %q: %w", model.Name, err)
		}
		merged[model.Name] = entry
	}
	return merged, nil
}
