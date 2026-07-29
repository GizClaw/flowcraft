// Package bytedance implements the ByteDance (Volcengine Ark + Doubao speech)
// provider for the unified inference runtime. It owns the provider's model
// catalog, strict Spec decoding, and all wire compilers; sdk/inference never
// sees ByteDance concepts.
//
// Operation coverage:
//
//   - Generate (unary + stream): Ark Responses API — text, vision input,
//     tool calling, reasoning effort, JSON/JSON-schema response formats.
//   - Generate ImageIntent: Ark images.generations (seedream models).
//   - Generate AudioIntent: Doubao TTS 2.0 HTTP chunked synthesis (seed-tts).
//   - Embed: Ark embeddings (doubao-embedding text; multimodal embedding for
//     vision-capable variants).
//   - Transcription session: Doubao ASR V2 SAUC WebSocket streaming. Unary
//     Transcribe is intentionally absent: Volcengine only offers asynchronous
//     file recognition, which cannot satisfy the synchronous unary contract.
//   - Realtime: Doubao full-duplex dialogue (Seeduplex) over WebSocket JSON.
//
// Provider-specific settings ride on requests as typed extensions, one
// options struct per operation family: GenerateOptions (thinking switch,
// service tier, context caching, server-side storage and chaining, tool-call
// limits, web search), ImageOptions (guidance scale, watermark, prompt
// optimization, grouped generation, named size tiers, web search), TTSOptions
// (pitch, volume, emotion, compressed bitrate), TranscriptionOptions
// (diarization, hotwords, result type, ITN and punctuation switches), and
// RealtimeOptions (output speed and loudness). Every extension field is
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
// Credentials come exclusively from config profiles: `api_key` authenticates
// Ark and is the default speech credential, `speech_api_key` optionally
// overrides it for speech services, and the profile Spec's `app_id` supplies
// the Doubao speech application ID those services still require.
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
// speech_base_url, speech_web_socket_url), rewrites addressed models
// (endpoints), declares extra models (models), and profile Specs supply
// per-credential app_id. See integration_test.go for a worked example.
package bytedance
