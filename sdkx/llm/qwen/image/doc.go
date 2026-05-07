// Package image is the Alibaba DashScope Qwen-Image text-to-image
// adapter. It targets the synchronous multimodal-generation endpoint
// (POST /api/v1/services/aigc/multimodal-generation/generation, see
// https://help.aliyun.com/zh/model-studio/qwen-image-api) and exposes
// it through the standard sdk/llm.LLM interface so callers can route
// image generation through the same resolver, fallback, and
// credential-profile machinery as chat models.
//
// # Input mapping
//
// The endpoint is text-to-image only and accepts exactly one user
// message with one text content part per the upstream contract
// ("当前仅支持单轮对话" / "仅支持传入一个text"). The adapter therefore:
//
//   - Concatenates all user-message text parts into a single text.
//   - Drops system and assistant messages.
//   - Rejects [model.PartImage] inputs with a validation error.
//
// Image editing is served by a separate qwen-image-edit endpoint not
// handled by this adapter; rejecting reference images keeps callers
// from silently getting a t2i result that ignored their references.
//
// # Image options
//
// Image-specific knobs flow through [llm.GenerateOptions.ImageGen]:
//
//   - Width + Height map to parameters.size in "W*H" form.
//   - N maps to parameters.n.
//   - Seed maps to parameters.seed.
//
// Note the unusual "*" separator: Qwen uses "W*H", not the "WxH"
// convention shared by OpenAI / Seedream / MiniMax. AspectRatio and
// ResponseFormat are silently ignored: this endpoint has no
// aspect-ratio knob (use Width/Height) and only returns URL.
//
// Per-model n limits are enforced server-side: 2.0 series accept 1-6,
// max / plus / qwen-image are fixed at 1.
//
// # Provider extras
//
// Provider-specific knobs are passed through [llm.WithExtra]:
//
//   - "negative_prompt" (string) discouraged content guidance.
//   - "prompt_extend" (bool) toggles the model's auto rewrite, default true upstream.
//   - "watermark" (bool) adds the "Qwen-Image" watermark.
//   - "size" (string) overrides Width/Height-derived size, useful for the max / plus fixed-size SKUs.
//
// # Output mapping
//
// The response (output.choices[0].message.content[].image) is parsed
// into a single assistant [model.Message] whose Parts are
// [model.PartImage] entries, one per generated image. Token usage is
// left zero — this endpoint reports image_count and dimensions
// rather than token counts.
//
// # Streaming
//
// The endpoint is synchronous; GenerateStream wraps Generate via
// [llm.NewOneChunkStream] so the [llm.StreamMessage] contract holds.
//
// # Authentication and regions
//
// Bearer token in the Authorization header. The default base URL is
// the Beijing endpoint https://dashscope.aliyuncs.com; deployments
// routing through the international (Singapore) region should set
// base_url to https://dashscope-intl.aliyuncs.com. Beijing and
// Singapore have independent API keys and are NOT cross-callable.
//
// The provider key is "qwen-image" so existing "qwen" chat
// credentials remain independent.
package image
