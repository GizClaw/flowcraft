// Package image is the ByteDance Doubao-Seedream image-generation
// adapter. It targets Volcengine Ark's POST /api/v3/images/generations
// endpoint (see https://www.volcengine.com/docs/82379/1824121) and
// exposes it through the standard sdk/llm.LLM interface so callers
// can route image generation through the same resolver, fallback,
// and credential-profile machinery as chat models.
//
// # Input mapping
//
// The adapter translates a [model.Message] list into a single
// Seedream request:
//
//   - The last user message's text parts are concatenated into the prompt field.
//   - All [model.PartImage] entries across user messages are forwarded as the image field.
//   - System and assistant messages are dropped.
//
// Seedream recommends prompts ≤300 Chinese chars / 600 English words
// but does not hard-fail on longer prompts. The image field is
// serialized as a JSON string when there is exactly one reference
// and as a JSON array when there are multiple; with zero references
// the call degrades to text-to-image (still served by the same
// endpoint).
//
// # Image options
//
// Image-specific knobs flow through [llm.GenerateOptions.ImageGen]:
//
//   - Width + Height map to size in "WxH" form.
//   - N > 1 maps to sequential_image_generation = "auto" plus sequential_image_generation_options.max_images = N.
//   - ResponseFormat maps to response_format ("url" or "b64_json").
//
// AspectRatio and Seed are ignored by the Seedream surface.
//
// # Provider extras
//
// Callers needing the "2K" / "3K" / "4K" preset path or the
// watermark / optimize / web_search / output_format knobs pass them
// via [llm.WithExtra]:
//
//   - "size" (string) overrides Width/Height-derived size.
//   - "watermark" (bool) toggles the "AI生成" watermark.
//   - "output_format" (string) "png" or "jpeg" (5.0-lite only).
//   - "optimize_prompt_mode" (string) "standard" or "fast" (4.0 only).
//   - "web_search" (bool) wraps {tools: [{type: "web_search"}]} (5.0-lite only).
//
// # Output mapping
//
// The response is parsed into a single assistant [model.Message]
// whose Parts are [model.PartImage] entries (one per generated
// image), preserving the URL or b64_json delivery format.
//
// # Streaming
//
// Seedream supports native server-side streaming via SSE, but the
// stream chunks carry image payloads which the text-oriented
// [llm.StreamChunk] cannot express. This adapter therefore wraps
// Generate via [llm.NewOneChunkStream] — the call is still
// synchronous from the caller's perspective. Native streaming can
// be added later by extending the StreamChunk envelope.
//
// # Authentication
//
// Bearer token in the Authorization header. The provider key is
// "bytedance-image" so existing "bytedance" chat credentials remain
// independent (the Volc Ark account is the same but the rate-limit
// pools are tracked separately upstream).
package image
