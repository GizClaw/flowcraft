// Package openai implements the OpenAI provider for the unified inference
// runtime. It owns the provider's model catalog, strict Spec decoding, and
// all wire compilers; sdk/inference never sees OpenAI concepts.
//
// Operation coverage:
//
//   - Generate (unary + stream): Responses API — text, vision input, tool
//     calling, reasoning effort, JSON/JSON-schema response formats.
//     Reasoning models cannot switch reasoning off: reasoning_enabled:
//     false rejects at compile time; true is a no-op (the default).
//     Reasoning items decode into canonical reasoning parts (summary text,
//     encrypted payload in the Signature slot, item id) and round-trip
//     through context when id and payload survive; the request always
//     includes reasoning.encrypted_content so round-trips stay possible.
//   - Generate ImageIntent: Images API (gpt-image models).
//   - Generate AudioIntent: speech API (gpt-4o-mini-tts and friends).
//   - Embed: embeddings API (text-embedding-3 family).
//   - Transcription: audio transcriptions API (gpt-4o-transcribe family),
//     unary only — the endpoint takes complete files, so no streaming
//     session exists to drive.
//
// Realtime (gpt-realtime) is intentionally absent: the pinned openai-go has
// no WebSocket coverage and this environment's module proxy cannot serve a
// newer SDK, so the GA protocol would be hand-rolled. It lands with a
// capable SDK or an accepted protocol dependency.
//
// Credentials come exclusively from config profiles: `api_key` authenticates
// every OpenAI surface. The provider Spec redirects transport (base_url),
// scopes requests (organization, project), and declares extra models
// (models); the profile Spec is reserved and currently carries no settings.
//
// Deployments wire this package through the config builder under the driver
// name "openai":
//
//	builder, _ := config.NewBuilder(
//		map[string]config.Factory{"openai": openai.Factory()},
//		map[string]config.SecretResolver{"env": env.New()},
//	)
//	assembly, _ := builder.NewAssembly(ctx, document) // Runtime + Router
package openai
