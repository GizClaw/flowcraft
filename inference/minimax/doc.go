// Package minimax implements MiniMax's media surfaces for the unified inference
// runtime: speech synthesis on t2a_v2 and video generation on the v2
// MiniMax-H3 task pipeline.
//
// # Protocol
//
// MiniMax serves an Anthropic-compatible Messages API, and that surface is
// owned by the anthropic driver: its thinking blocks carry verification
// signatures that must round-trip verbatim, which is exactly the Messages
// protocol's reasoning policy, so the driver that speaks Messages keeps the
// kernel for it. This package serves the media APIs, which are plain JSON with
// Bearer auth and a base_resp status envelope on the v1 surfaces:
//
//   - Speech synthesis: t2a_v2, one JSON request, audio back as hex — unary in
//     one payload, streaming as server-sent events of the same shape.
//   - Video generation: the async v2 task pipeline (create, poll, then surface
//     the finished artifact's URL).
//
// Both surfaces are the platform's own shapes, so BaseURL points at a MiniMax
// media root (api.minimaxi.com or api.minimax.io). A gateway that normalizes
// speech onto the OpenAI wire — POST /v1/audio/speech with {model, input,
// voice} and raw audio back — is not this driver: that is the OpenAI speech
// surface, served by core/inference-openai. A gateway that forwards the video
// task API unchanged (/v2/video_generation plus its query endpoint) needs no
// adapter at all, because that is already this driver's wire.
//
// The v1 video API the Hailuo 2.x/02 trio rides is deliberately not served.
// Only MiniMax-H3 is declared here, and its API differs in every dimension that
// matters — multimodal content roles instead of flat first/last-frame fields,
// 768P/2K instead of 768P/1080P/512P, 4-15s instead of 6s/10s, and a ratio
// control the v1 API does not have. Compiling one family onto the other would
// be a silent lie, so a Hailuo declaration fails closed at the endpoint instead.
//
// # Models
//
// The driver ships no model line-up: every media model is a deployment
// declaration, and a name no declaration covers fails closed with
// unknown_model. A declaration states its kind ("tts" or "video"), its
// capabilities, and — for speech — the payload defaults the endpoint should
// apply when a call names none.
//
// # Behavior mapping
//
//   - Speech voice, language, speed, encoding, sample rate and channel layout
//     ride the TTSOptions extension, because the canonical request models no
//     voice at all. The voice is required: t2a_v2 refuses a request without
//     one. Speed maps only inside MiniMax's [0.5, 2] band, so a rate outside it
//     is rejected rather than clamped. pcm output is 16-bit, so PCM24,
//     float32 and AAC encodings reject, and pcm16 requires an explicit sample
//     rate and channel count whether they come from the call or the
//     declaration. Voice language rides language_boost verbatim, defaulting to
//     "auto".
//   - Streamed speech emits one chunk part per SSE audio event and closes on
//     the terminal status. The negotiated format is fixed at compile time and
//     travels with every chunk, so the response describes its payload without
//     decoding it.
//   - Video generation is MiniMax-H3 only: a multimodal content array (text
//     plus image/video/audio items carrying first_frame, last_frame and
//     reference_* roles), durations of 4-15 whole seconds, 768P or 2K, and a
//     ratio control. Text-only tasks must name a non-adaptive ratio and default
//     to 16:9; image and reference tasks default to adaptive. First/last-frame
//     input and reference input are mutually exclusive. The API has no seed
//     knob, so nothing offers one.
//   - Video output URLs expire one hour after retrieval. The driver returns the
//     URL verbatim; downloading and persisting is the caller's job.
//
// # Retries
//
// The settings accept a poll interval for the video task pipeline, an HTTP
// client or timeout, and a retries count. Retries are left at zero by default
// so the route Router owns the whole budget; a non-zero value belongs to a
// deployment whose gateway is the only retry layer.
//
// The budget only covers reads. Every paid surface here is a create — a video
// task and a speech synthesis — and both are billed per call, so re-sending one
// after a transient failure would buy the work twice: the server may already
// have accepted the request when the response was lost. The video task poll is
// a read and does retry, which is where the budget earns its keep.
package minimax
