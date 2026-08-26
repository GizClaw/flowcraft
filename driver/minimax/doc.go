// Package minimax implements the MiniMax provider for the unified
// inference runtime.
//
// # Protocol
//
// MiniMax serves an Anthropic-compatible Messages API
// (https://api.minimaxi.com/anthropic for China,
// https://api.minimax.io/anthropic internationally). Its thinking blocks
// carry verification signatures that must round-trip verbatim — exactly
// the Messages protocol's reasoning policy — so the generate pipeline
// hosts its own copy of the Messages kernel (compiler, transports, and
// decoders) rather than importing another provider driver. The provider's
// OpenAI-compatible surface is deliberately not used: there, thinking
// round-trips as free-form reasoning_details with no signature
// verification, a strictly weaker contract.
//
// The OpenAI-compatible surfaces (Chat Completions and Responses) were
// evaluated and rejected: their reasoning round-trips are free-form text
// (reasoning_details, reasoning summary segments) with no signature or
// encrypted content, a strictly weaker contract than signed thinking
// blocks; the Responses effort levels (minimal/low/medium/high) are
// accepted for compatibility but do not tune reasoning depth, so the
// control surface is the same binary thinking with a misleading shape;
// and their reasoning items cannot feed the openai kernel (no
// item_reference, no encrypted_content), so reuse would cost the same
// dialect knob for a weaker guarantee.
//
// Beyond Messages, the provider serves MiniMax's media APIs over plain
// JSON with a base_resp status envelope:
//
//   - Generate AudioIntent: t2a_v2 speech synthesis (speech-2.8/2.6/02
//     HD and turbo tiers), unary hex payloads and SSE hex streaming.
//   - Generate ImageIntent: image_generation (image-01, image-01-live),
//     unary; URL deliveries expire after 24 hours on the provider side.
//   - Generate VideoIntent: the async video task pipeline. The Hailuo
//     2.3/2.3-Fast/02 trio rides the v1 API (create, poll, retrieve; the
//     download URL expires one hour after retrieval); MiniMax-H3 rides the
//     v2 API and returns the artifact URL from the query directly.
//   - Generate TextIntent on MiniMax-H3-Context-IR: the v2 H3-Context-IR
//     task returns an enhanced video prompt (text), never a video. The
//     target duration/ratio ride the ContextIROptions extension.
//   - Generate AudioIntent without a voice: music_generation
//     (music-3.0/2.6 and their -free tiers). The canonical voice is
//     optional: speech models require one, music models reject one.
//     Lyrics, the instrumental switch, and the AIGC watermark ride the
//     MusicOptions extension. music-cover stays out: its reference-audio
//     plus two-step cover_feature_id flow has no canonical surface.
//
// # Catalog
//
// The built-in catalog covers the Anthropic-endpoint lineup as of
// 2026-07: MiniMax-M3 (1M context, image input) and the M2.x series
// (M2.7, M2.5, M2.1 and their highspeed twins, plus M2 — 204,800
// context, text-only), the speech-2.8/2.6/02 speech models, image-01 and
// image-01-live, the Hailuo video trio plus MiniMax-H3, the
// MiniMax-H3-Context-IR prompt-enhancement task, and the music-3.0/2.6
// music models. Custom models declare
// through the spec's models list; unknown channels stay fail closed.
//
// # Behavior mapping
//
//   - Thinking is binary on this surface — no effort levels. The
//     canonical switch maps directly: reasoning_enabled: true emits
//     thinking: {type: "adaptive"}, and false emits disabled on
//     MiniMax-M3 while the always-thinking M2.x series rejects it at
//     compile time. A requested reasoning effort turns thinking on and
//     lands Dropped on the ledger — the level itself cannot be honored.
//     With no reasoning intent, nothing is sent: M3 stays non-thinking
//     by default.
//   - Thinking blocks decode into canonical reasoning parts with their
//     signature; assistant turns hoist them first when context compiles
//     back, and unsigned reasoning drops — the self-hosted kernel's
//     policy, which matches MiniMax's requirement to preserve thinking
//     blocks unchanged across tool-use turns.
//   - Image input compiles for MiniMax-M3 (URL or base64) and rejects on
//     the text-only M2.x series. Video input has no wire support in the
//     kernel and rejects everywhere for now.
//   - MiniMax ignores top_k and stop_sequences on this endpoint; the
//     canonical request has no surface for either, so nothing is lost.
//   - service_tier (standard/priority) is MiniMax-specific and has no
//     canonical or extension surface yet.
//   - Speech speed maps only inside MiniMax's [0.5, 2] band; pcm output
//     is 16-bit, so PCM24/float32/AAC encodings reject at compile time.
//     Voice language rides language_boost verbatim.
//   - Image output is always JPEG, so other requested formats reject.
//     Image parts compile into character subject references
//     (image-to-image); custom sizes must be 512–2048 and divisible
//     by 8.
//   - Video durations are model-bound. The Hailuo trio serves 6s, and 10s
//     at 768P on the 2.3/2.3-Fast/02 models; Hailuo-2.3-Fast is
//     image-to-video only, and Hailuo-02 adds 512P (single-first-frame
//     image-to-video only; first/last-frame tasks cap at 768P/1080P) and
//     first/last-frame input. MiniMax-H3 rides the v2 API: 4-15s
//     durations, 768P/2K, a ratio knob (text-only tasks default to 16:9
//     and reject adaptive), and multimodal reference inputs. The v1-only
//     prompt_optimizer / fast_pretreatment controls and the shared
//     callback_url ride the VideoOptions extension, which also carries
//     last_frame_only to mark a single v2 input image as the closing
//     frame. The task APIs have no seed knob — it rejects.
//   - H3-Context-IR shares the v2 content roles and the 4-15s duration /
//     ratio rules, but returns an enhanced video prompt as text; the
//     target duration/ratio ride ContextIROptions.
//   - Music encodes mp3 or 16-bit pcm at 16/24/32/44.1kHz; bitrate and
//     channel layout are model-fixed, so those canonical knobs reject.
//     Speech synthesis requires a voice; music generation requires none.
//
// # Wiring
//
// Wire the factory into the deployment config by provider id:
//
//	builder := config.NewBuilder(
//		map[string]config.Factory{"minimax": minimax.Factory()},
//		resolvers,
//	)
//
// Each profile resolves one api_key secret; the spec optionally overrides
// base_url (Anthropic endpoint) and media_base_url (media API root, which
// defaults to base_url with the /anthropic suffix trimmed), paces video
// polling, and declares extra models.
//
// # Retries
//
// The provider Spec accepts `http_retries` (total wire attempts including
// the first). It maps to option.WithMaxRetries on the Anthropic Messages
// client and to the httpkit retry budget on the media client. Nil keeps the
// SDK/httpkit defaults; 0 disables provider-level retries so the route
// Router owns the budget. Retry-After and wire attempts are propagated onto
// ProviderFailure so Router backoff and trace wire_attempts can observe
// them.
package minimax
