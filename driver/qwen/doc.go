// Package qwen adapts Alibaba Model Studio's native DashScope generation
// API onto the unified inference runtime.
//
// # Protocol
//
// The driver speaks the native DashScope protocol (NOT the OpenAI
// compatible-mode endpoint): POST {base}/api/v1/services/aigc/
// text-generation/generation for text models and .../multimodal-
// generation/generation for the vision/video models, Bearer auth, and SSE
// streams gated by the X-DashScope-SSE: enable header. Streams always
// compile with incremental_output=true because the canonical stream
// surface is delta-shaped. Responses decode from the {output.choices[0].
// message, usage} envelope; failures classify from the HTTP status plus
// the body's code/message pair (and from a non-empty code on a 200
// envelope, which SSE streams can produce).
//
// # Reasoning
//
// The commercial hybrid-thinking models (qwen3.7/qwen3.8, Qwen3-VL) map
// ReasoningEnabled onto enable_thinking and emit reasoning_content traces.
// Two protocol constraints shape the compile:
//
//   - Thinking mode is stream-only server-side, so a unary compile with
//     thinking on rejects generate.input.content.intent.text.
//     reasoning_enabled.
//   - Effort levels (reasoning_effort) exist only on qwen3.8-max-preview
//     (low/medium/xhigh per the DashScope docs); the canonical high
//     resolves to xhigh (DashScope's top level) and the quantization is
//     reported as a drop. On other thinking models an explicit effort
//     drops with a reason and the GenerateOptions.ThinkingBudget
//     extension bounds the trace instead.
//
// Reasoning history round-trips through preserve_thinking: assistant
// reasoning parts compile to reasoning_content on models that declare the
// capability, preserve_thinking defaults on when the history carries a
// trace (GenerateOptions.PreserveThinking overrides), and traces drop with
// a reason on models that cannot re-ingest them.
//
// # Multimodal input
//
// Vision entries take image parts as URLs or data URIs and video parts as
// URLs (frame lists and pixel-budget controls such as fps / max_frames /
// min_pixels are DashScope-specific and not yet surfaced); text-generation
// entries reject image and video parts.
//
// # Embeddings
//
// Two DashScope embedding models ride this driver: text-embedding-v4 on
// /api/v1/services/embeddings/text-embedding/text-embedding (text only,
// at most 10 rows per batch) and qwen3-vl-embedding on
// .../embeddings/multimodal-embedding/multimodal-embedding (text, image,
// video; Beijing region only). The multimodal compiler batches
// single-part items into one independent-vector call and switches to
// per-item enable_fusion calls as soon as an item carries multiple parts,
// since the API fuses a whole request into a single vector. Sparse
// output (text-embedding-v3/v4 output_type=sparse) has no canonical
// representation and stays at dense. EmbedOptions carries the two
// DashScope-only settings: the query/document text_type (text-embedding
// only) and the task instruct.
//
// # Catalog
//
// The built-in catalog declares the commercial lineup: the qwen3.7 /
// qwen3.8 hybrid-thinking multimodal models, the Qwen3-VL pair, and the
// legacy text-only qwen-plus / turbo / flash / max, plus the two
// embedding models above. Deployments declare additional models through
// the spec (name, kind, capabilities). Media output (image /
// speech / video generation) lives in dedicated DashScope SKUs, not this
// driver.
//
// # Retries
//
// The provider Spec accepts `http_retries` (total wire attempts including
// the first). Nil keeps the httpkit default (three attempts); 0 disables
// transport retries so the route Router owns the budget; N sets total wire
// attempts. Retry-After and wire attempts observed at the HTTP layer are
// propagated onto ProviderFailure so Router backoff and trace
// wire_attempts can observe them.
package qwen
