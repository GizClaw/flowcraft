// Package bytedance implements the ByteDance (Volcengine Ark) provider for
// the unified inference runtime. It owns the provider's model catalog, strict
// Spec decoding, and all wire compilers; sdk/inference never sees ByteDance
// concepts.
//
// Operation coverage:
//
//   - Generate (unary + stream): Ark Responses API — text, vision input,
//     tool calling, reasoning effort, JSON/JSON-schema response formats.
//     Reasoning items decode into canonical reasoning parts (summary text
//     plus item id); ark signs nothing and consumes no reasoning input, so
//     traces never round-trip and context compiles them as Dropped.
//     GenerateOptions.WebSearch attaches Ark's hosted web_search tool;
//     web_search_call items and url_citation annotations surface on
//     GenerateResponse.ProviderOutputs (never inside Message).
//   - Generate ImageIntent: Ark images.generations (seedream models).
//   - Generate VideoIntent: Ark content-generation tasks (seedance models).
//     The API is asynchronous — the transport creates a task and polls it to
//     a terminal state inside the caller's context, so the unary contract
//     holds. Input images map to first/last-frame roles; the 2.0 series and
//     2.5 additionally accept reference images (3+), reference videos, and
//     reference audio under the official mutual-exclusion rules. Parameters
//     the model does not support (seed, camera_fixed, flex tier, duration
//     bounds) are rejected at compile time per the official per-model
//     support matrix.
//   - Embed: Ark embeddings (doubao-embedding text; multimodal embedding for
//     vision-capable variants).
//
// Provider-specific settings ride on requests as typed extensions, one
// options struct per operation family: GenerateOptions (service tier,
// context caching, server-side storage and chaining, tool-call limits,
// web search), ImageOptions (guidance scale, watermark, prompt
// optimization, grouped generation, named size tiers, web search),
// VideoOptions (fixed camera, generated audio track, service tier, task
// TTL). Every extension field is
// consumed explicitly by the matching compiler; an extension attached to the
// wrong operation, or one colliding with a canonical channel, is rejected
// with InvalidExtension.
//
// Extensions are addressed by provider: each options struct carries an
// optional Provider field (defaulting to this driver) that targets one
// deployment provider ID. Extensions addressed to other providers stay
// inert on bytedance attempts — and vice versa — so a request routed with
// fallback across providers may carry several providers' options at once;
// only the executing provider's settings apply to each attempt.
//
// Credentials come exclusively from config profiles: `api_key` authenticates
// Ark.
//
// Deployments wire this package through the config builder under the driver
// name "bytedance":
//
//	builder, _ := config.NewBuilder(
//		map[string]config.Factory{"bytedance": bytedance.Factory()},
//		map[string]config.SecretResolver{"env": env.New()},
//	)
//	assembly, _ := builder.NewAssembly(ctx, document) // Runtime + Router
//
// The provider Spec in the document redirects transport (base_url), declares
// extra models (models), and tunes Seedance task polling
// (video_poll_interval_millis). Profile Specs carry the account-scoped
// settings: the endpoints map binding model names to that account's Ark
// endpoint IDs (ep-xxx). Unmapped models are addressed by their catalog name.
// See integration_test.go for a worked example.
//
// # Retries
//
// The provider Spec accepts `http_retries` (total wire attempts including
// the first). It governs the Ark HTTP client through the shared httpkit
// transport. Nil keeps the httpkit default (three attempts); 0 disables
// transport retries so the route Router owns the budget; N sets total wire
// attempts. The Ark SDK error type does not expose response headers, so
// Retry-After is honored inside the httpkit transport but is not visible to
// Router backoff; wire attempts are likewise not reported through the SDK
// error chain.
package bytedance
