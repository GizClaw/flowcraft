// Package kimi adapts Moonshot AI's Kimi API (platform.kimi.com) onto the
// unified inference runtime.
//
// # Protocol
//
// The driver speaks Kimi's OpenAI-compatible chat completions protocol:
// POST {base}/chat/completions with Bearer auth (MOONSHOT_API_KEY), where
// {base} defaults to https://api.moonshot.cn/v1. Requests carry stream:
// true for streaming; the SSE wire emits delta-shaped chunks and a [DONE]
// sentinel, with usage on the final chunk when stream_options.
// include_usage is set (the driver always sets it). Errors classify from
// the HTTP status plus the body's error.type/error.message pair.
//
// The surface is OpenAI-shaped but not OpenAI-typed: Kimi owns fields the
// openai-go SDK does not model (thinking, reasoning_effort, video_url
// content parts, prompt_cache_key), so the driver renders the request
// body itself from fully concrete wire types.
//
// # Reasoning dialects
//
// Three model families think differently, and the compiler maps the
// canonical ReasoningEnabled / ReasoningEffort onto each:
//
//   - kimi-k3 always thinks and always preserves history reasoning. It
//     takes top-level reasoning_effort ("low" | "high" | "max", default
//     "max"); the canonical medium quantizes to high and the loss is
//     reported as a drop. ReasoningEnabled=false rejects — the model has
//     no off switch.
//   - kimi-k2.7-code (and highspeed) always thinks with thinking.type
//     fixed at "enabled" and thinking.keep fixed at "all".
//     ReasoningEnabled=false rejects; effort drops with a reason.
//   - kimi-k2.6 / kimi-k2.5 toggle thinking via thinking.type
//     ("enabled" | "disabled"). kimi-k2.6 additionally accepts
//     thinking.keep="all" (Preserved Thinking): assistant reasoning parts
//     round-trip as reasoning_content, and the compiler defaults keep to
//     "all" when history carries a trace (GenerateOptions.
//     PreserveThinking overrides). kimi-k2.5 cannot re-ingest traces, so
//     history reasoning drops with a reason. moonshot-v1 has no thinking
//     control at all and rejects both knobs.
//
// # Multimodal input
//
// Vision entries take image parts as URLs or Base64 data URIs (Kimi also
// accepts ms:// file ids); kimi-k3 additionally takes video parts in the
// same two forms. Text-only entries reject image and video parts.
// moonshot-v1's vision previews ride the same content-array shape.
//
// # Sampling split
//
// temperature / top_p exist on the moonshot-v1 family only — the K3 /
// K2.x request schemas carry no sampling knobs, so those fields drop with
// a reason on the K models and compile natively on moonshot-v1.
//
// # Catalog
//
// The built-in catalog declares the public lineup: kimi-k3, the
// kimi-k2.7-code pair, kimi-k2.6, kimi-k2.5, and the moonshot-v1 family
// (text plus vision previews). Deployments declare additional models
// through the spec (name, kind, vision, video, reasoning).
//
// # Retries
//
// The provider Spec accepts `http_retries` (total wire attempts including
// the first). Nil keeps the httpkit default (three attempts); 0 disables
// transport retries so the route Router owns the budget; N sets total wire
// attempts. Retry-After and wire attempts observed at the HTTP layer are
// propagated onto ProviderFailure so Router backoff and trace
// wire_attempts can observe them.
package kimi
