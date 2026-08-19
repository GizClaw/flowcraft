// Package bytedance implements the ByteDance (Volcengine Ark + Doubao speech)
// provider for the unified inference runtime. It owns the provider's model
// catalog, strict Spec decoding, and all wire compilers; sdk/inference never
// sees ByteDance concepts.
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
//     holds. Input images map to first/last-frame roles; video-reference
//     input awaits SDK support and is rejected truthfully.
//   - Generate AudioIntent: Doubao TTS 2.0 HTTP chunked synthesis (seed-tts).
//   - Embed: Ark embeddings (doubao-embedding text; multimodal embedding for
//     vision-capable variants).
//   - Transcription: Doubao ASR V2 SAUC WebSocket recognition (seed-asr).
//     Whole-file Transcribe feeds the complete inline audio into a streaming
//     session inside the transport; TranscribeSession is the duplex live
//     form with continuous multi-utterance partial/final events, word
//     timing, and barge-in Interrupt. Sessions stay open across finals;
//     explicit end-of-input (FinishInput / the bridge's finish()) sends the
//     ASR final packet so draining terminates and Result returns the
//     accumulated transcript. Realtime is intentionally absent:
//     core/inference does not expose that operation surface yet.
//
// Provider-specific settings ride on requests as typed extensions, one
// options struct per operation family: GenerateOptions (service tier,
// context caching, server-side storage and chaining, tool-call limits,
// web search), ImageOptions (guidance scale, watermark, prompt
// optimization, grouped generation, named size tiers, web search),
// VideoOptions (fixed camera, generated audio track, service tier, task
// TTL), TTSOptions (pitch, volume, emotion, compressed bitrate). Every
// extension field is
// consumed explicitly by the matching compiler; an extension attached to the
// wrong operation, or one colliding with a canonical channel, is rejected
// with InvalidExtension. Settings that cannot coexist with the canonical
// contract — the TTS mixed-speaker mode, whose voice replacement conflicts
// with the mandatory canonical voice — are deliberately not exposed.
//
// Extensions are addressed by provider: each options struct carries an
// optional Provider field (defaulting to this driver) that targets one
// deployment provider ID. Extensions addressed to other providers stay
// inert on bytedance attempts — and vice versa — so a request routed with
// fallback across providers may carry several providers' options at once;
// only the executing provider's settings apply to each attempt.
//
// Credentials come exclusively from config profiles. `api_key`
// authenticates Ark and is the default speech credential, `speech_api_key`
// optionally overrides it for speech services, and the profile Spec's
// `app_id` supplies the Doubao speech application ID those services still
// require. As an alternative to `api_key`, the `access_key`/`secret_key`
// pair authenticates Ark via Volcengine IAM AK/SK signing; the two schemes
// are mutually exclusive within one profile. AK/SK has no channel for
// images or content-generation tasks (the SDK hard-fails them) and none for
// speech services, so image/video drivers reject AK/SK profiles at open
// time and speech still needs an API-key profile.
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
// The provider Spec in the document redirects transport (base_url,
// speech_base_url, speech_web_socket_url), declares extra models (models),
// and tunes Seedance task polling (video_poll_interval_millis). Profile
// Specs carry the account-scoped settings: app_id and the endpoints map
// binding model names to that account's Ark endpoint IDs (ep-xxx), speech
// resource IDs, or — for realtime models — the duplex engine version.
// Unmapped models are addressed by their catalog name. See
// integration_test.go for a worked example.
//
// # Retries
//
// The provider Spec accepts `http_retries` (total wire attempts including
// the first). It governs the Ark and Doubao speech HTTP clients through the
// shared httpkit transport. Nil keeps the httpkit default (three attempts);
// 0 disables transport retries so the route Router owns the budget; N sets
// total wire attempts. The Ark and speech SDK error types do not expose
// response headers, so Retry-After is honored inside the httpkit transport
// but is not visible to Router backoff; wire attempts are likewise not
// reported through the SDK error chain.
package bytedance
