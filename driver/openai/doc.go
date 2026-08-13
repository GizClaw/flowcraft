// Package openai implements the OpenAI provider for the unified inference
// runtime. It owns the provider's model catalog, strict Spec decoding, and
// all wire compilers; core/inference never sees OpenAI concepts.
//
// Operation coverage:
//
//   - Generate (unary + stream): Responses API by default — text, vision input, tool
//     calling, reasoning effort, JSON/JSON-schema response formats.
//     Set provider settings `api: chat` to switch a provider instance to
//     Chat Completions. Chat mode drops Responses-only reasoning
//     round-trips and rejects the hosted web_search tool at compile time;
//     the rest of the generate surface (tools, vision, JSON formats,
//     streaming) is equivalent.
//     Reasoning models cannot switch reasoning off: reasoning_enabled:
//     false rejects at compile time; true is a no-op (the default).
//     Reasoning items decode into canonical reasoning parts (summary text,
//     encrypted payload in the Signature slot, item id) and round-trip
//     through context when id and payload survive; the request always
//     includes reasoning.encrypted_content so round-trips stay possible.
//     GenerateOptions.WebSearch attaches OpenAI's hosted web_search tool;
//     web_search_call items and url_citation annotations surface on
//     GenerateResponse.ProviderOutputs (never inside Message).
//   - Generate ImageIntent: Images API (gpt-image models).
//   - Generate AudioIntent: speech API (gpt-4o-mini-tts and friends).
//   - Embed: embeddings API (text-embedding-3 family).
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
// Transcription (gpt-4o-transcribe family) is also absent for now:
// core/inference does not expose the transcription operation surface yet.
//
// Deployments wire this package as an inference.Provider resource:
//
//	reg.MustRegister(openai.Factory())
//
// and reference it from the inference assembly:
//
//	resources:
//	  provider.openai: {kind: inference.Provider, impl: openai, settings: {...}}
//	  infer: {kind: inference.Assembly, impl: unified, deps: {provider.openai: provider.openai}}
//
// # Retries
//
// The provider Spec accepts `http_retries` (total wire attempts including
// the first). Nil keeps the openai-go default (two retries); 0 disables
// SDK-internal retries so the route Router owns the budget; N maps to
// option.WithMaxRetries(N-1). Retry-After and the SDK retry count are
// propagated onto ProviderFailure so Router backoff and trace
// wire_attempts can observe them.
package openai
