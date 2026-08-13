package minimax

import (
	"fmt"
	"maps"
	"sort"
)

type modelKind string

const (
	kindGenerate modelKind = "generate"
	kindImage    modelKind = "image"
	kindTTS      modelKind = "tts"
	kindVideo    modelKind = "video"
	kindMusic    modelKind = "music"
)

// catalogEntry declares what one catalog model accepts. The zero value is
// the bare text surface: the compiler rejects every channel the entry
// omits, so a model never silently accepts a feature it may not serve.
type catalogEntry struct {
	kind modelKind
	// vision accepts image input parts (MiniMax-M3 only; M2.x is
	// text-only).
	vision bool
	// reasoning accepts the reasoning intent and emits thinking traces.
	// Every M-series model reasons; M3 keeps thinking off unless asked,
	// M2.x thinks unconditionally.
	reasoning bool
	// reasoningDisable can force thinking off: MiniMax-M3 honors
	// thinking: {type: "disabled"}, the M2.x series does not and rejects
	// a disable request at compile time.
	reasoningDisable bool
	// video10s accepts 10-second durations at 768P.
	video10s bool
	// videoHD accepts 1080P resolution.
	videoHD bool
	// videoI2VOnly marks image-to-video models: the request must carry a
	// first-frame image.
	videoI2VOnly bool
}

// catalog reflects MiniMax's lineup as of 2026-07. Sources:
//   - https://platform.minimaxi.com/docs/api-reference/api-overview
//   - https://platform.minimaxi.com/docs/api-reference/speech-t2a-http
//   - https://platform.minimaxi.com/docs/api-reference/video-generation-t2v
//   - https://platform.minimaxi.com/docs/api-reference/image-generation-t2i
//
// All generate entries speak the binary-thinking dialect: any requested
// reasoning effort compiles to thinking: {type: "adaptive"} — the endpoint
// has no effort levels. MiniMax-M3 holds a 1M token context; the M2.x
// series holds 204,800. Music generation (music-3.0) is deliberately
// absent: the canonical audio intent is voice-shaped and has no honest
// surface for lyrics/instrumental control.
var catalog = map[string]catalogEntry{
	"MiniMax-M3":             {kind: kindGenerate, vision: true, reasoning: true, reasoningDisable: true},
	"MiniMax-M2.7":           {kind: kindGenerate, reasoning: true},
	"MiniMax-M2.7-highspeed": {kind: kindGenerate, reasoning: true},
	"MiniMax-M2.5":           {kind: kindGenerate, reasoning: true},
	"MiniMax-M2.5-highspeed": {kind: kindGenerate, reasoning: true},
	"MiniMax-M2.1":           {kind: kindGenerate, reasoning: true},
	"MiniMax-M2.1-highspeed": {kind: kindGenerate, reasoning: true},
	"MiniMax-M2":             {kind: kindGenerate, reasoning: true},

	// Speech synthesis (t2a_v2): HD and turbo tiers.
	"speech-2.8-hd":    {kind: kindTTS},
	"speech-2.8-turbo": {kind: kindTTS},
	"speech-2.6-hd":    {kind: kindTTS},
	"speech-2.6-turbo": {kind: kindTTS},
	"speech-02-hd":     {kind: kindTTS},
	"speech-02-turbo":  {kind: kindTTS},

	// Image generation.
	"image-01":      {kind: kindImage},
	"image-01-live": {kind: kindImage},

	// Video generation (async task API). Hailuo-2.3-Fast is image-to-video
	// only; the 2.3/02 pair runs 10s at 768P and 6s at 1080P.
	"MiniMax-Hailuo-2.3":      {kind: kindVideo, video10s: true, videoHD: true},
	"MiniMax-Hailuo-2.3-Fast": {kind: kindVideo, videoI2VOnly: true},
	"MiniMax-Hailuo-02":       {kind: kindVideo, video10s: true, videoHD: true},

	// Music generation (text-to-music; music-cover stays out — see
	// music.go). The -free tiers are rate-limited gratis twins.
	"music-3.0":      {kind: kindMusic},
	"music-3.0-free": {kind: kindMusic},
	"music-2.6":      {kind: kindMusic},
	"music-2.6-free": {kind: kindMusic},
}

// mergedCatalog overlays the built-in catalog with the spec's model
// declarations: a spec entry with a catalog name replaces that entry, and
// unknown names extend the catalog. Models stay fail closed — the factory
// only exposes what the merged catalog declares.
func mergedCatalog(spec Spec) (map[string]catalogEntry, error) {
	models := make(map[string]catalogEntry, len(catalog)+len(spec.Models))
	maps.Copy(models, catalog)
	for _, declared := range spec.Models {
		entry := catalogEntry{
			kind:      modelKind(declared.Kind),
			vision:    declared.Vision,
			reasoning: declared.Reasoning,
		}
		if entry.kind == "" {
			if existing, exists := models[declared.Name]; exists {
				entry.kind = existing.kind
			} else {
				entry.kind = kindGenerate
			}
		}
		if err := entry.validate(); err != nil {
			return nil, fmt.Errorf("model %q: %w", declared.Name, err)
		}
		models[declared.Name] = entry
	}
	return models, nil
}

func (e catalogEntry) validate() error {
	switch e.kind {
	case kindGenerate, kindImage, kindTTS, kindVideo, kindMusic:
		return nil
	default:
		return fmt.Errorf("unsupported kind %q", e.kind)
	}
}

// sortedNames returns catalog names in deterministic order so factory
// output is stable across runs.
func sortedNames(models map[string]catalogEntry) []string {
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
