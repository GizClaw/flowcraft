// Package dashscope implements the Alibaba Cloud Model Studio (百炼 /
// DashScope) provider for the unified inference runtime: the Wan (万相)
// media families, native text generation, and native text embeddings, all
// over the platform's own protocol.
//
// # Protocol
//
// The driver speaks the native DashScope protocol, not the OpenAI or
// Anthropic compatibility surfaces: the media and embedding families are
// native-only — the compatibility endpoints cannot express them at all —
// and text generation is served natively here because the native surface
// carries the platform's own controls (the thinking budget, hosted search)
// that the compatibility shim does not. Requests post to
//
//	{base}/api/v1/services/aigc/...
//
// with Bearer auth and, on the task surfaces, the mandatory
// X-DashScope-Async: enable header. Three protocol shapes exist:
//
//   - The async task pipeline. Image generation, every video surface, and
//     file transcription create a task (the create response carries
//     output.task_id), and the caller polls GET {base}/api/v1/tasks/{task_id}
//     until output.task_status reaches SUCCEEDED. Task data — including the
//     generated links — is retained for 24 hours and then cleared, so the
//     response carries the provider URL verbatim and downloading it is the
//     caller's job. File transcription rides the same pipeline but ends
//     differently: the terminal query names a result file rather than a
//     media link, and that one document the driver fetches itself, because
//     the transcript is the answer rather than a pointer at one.
//   - The synchronous multimodal call. Wan 2.6 and later image models post
//     one request to
//     {base}/api/v1/services/aigc/multimodal-generation/generation and
//     receive the result inline as a content array of text and image items.
//     Interleaved output (parameters.enable_interleave) additionally
//     requires the stream pair — X-DashScope-Sse: enable and
//     parameters.stream: true — because the platform refuses every other
//     combination, and answers as an event stream the driver folds back into
//     one result. Its failures arrive as an event carrying the error
//     envelope, sometimes under an HTTP 200, so the status alone does not
//     tell a finished run from a refused one.
//   - The synchronous generation and embedding calls. Text generation
//     posts to {base}/api/v1/services/aigc/text-generation/generation
//     (classic string content) or the multimodal-generation surface (typed
//     content entries, the Qwen3.5+ shape), and streams one envelope:
//     X-DashScope-Sse: enable pairs with parameters.incremental_output,
//     frames carry the increment, the terminal frame carries finish_reason
//     and the run's usage, and data: [DONE] closes the stream. Embeddings
//     post batches to
//     {base}/api/v1/services/embeddings/text-embedding/text-embedding,
//     whose output.embeddings[].text_index ties every vector back to its
//     slot. Refusals ride in band under an HTTP 200 on all of them, like
//     the interleaved image surface.
//
// Image models ride the text2image surface
// (.../aigc/text2image/image-synthesis), image-editing models the
// image2image surface (.../aigc/image2image/image-synthesis) with a
// function selector, and video models either the video-generation surface
// (.../aigc/video-generation/video-synthesis, text-to-video and
// image-to-video) or the image2video surface
// (.../aigc/image2video/video-synthesis, first/last-frame). Which surface a
// model speaks is declared per model, never inferred from its name.
//
// Failures arrive two ways: as an HTTP status plus a top-level
// {code, message, request_id} envelope on request errors, and as a
// terminal task_status of FAILED (with output.code/output.message) or
// CANCELED on the task surfaces. Both classify through errdefs, so
// providers/errors stay meaningful to callers and the route Router.
//
// # Surfaces
//
// The driver serves six kinds:
//
//   - kind "image": Wan text-to-image (wanx-v1, wan2.1, wan2.2, wan2.5,
//     wan2.6) and the image-editing families. The prompt comes from the
//     request's text parts, the canonical size/count/seed from the image
//     intent, and reference or edit images from image parts (editing reads
//     the first image as base_image_url and a second as mask_image_url).
//     Provider knobs no canonical field expresses — negative prompt,
//     prompt_extend, watermark, style, ref_strength/ref_mode, the edit
//     function, is_sketch, upscale_factor, enable_interleave — ride the
//     ImageOptions extension. Output images are PNG unless the model
//     declares another format; inline delivery is not offered, so an
//     inline/stream delivery intent rejects.
//   - kind "video": Wan text-to-video, image-to-video, and first/last-frame
//     video. The prompt comes from text parts, the first image becomes
//     img_url (i2v) or first_frame_url (kf2v, with a second image as
//     last_frame_url), audio parts become audio_url on models that declare
//     audio input, and resolution/duration/ratio/seed/watermark lower from
//     the video intent. Negative prompt, prompt_extend, shot_type, the
//     audio switch, the effect template, and the media surface's web page
//     and smart-duration knobs ride the VideoOptions extension. The
//     provider returns MP4 (H.264).
//
// A video declaration that names the media array (Wan 3.0's all-in-one
// models, wan3.0-video) switches the whole input side to input.media: typed
// entries in request order, taking first_frame/last_frame from the first and
// second image, reference_image/reference_video/reference_audio from image,
// video, and audio parts up to the declared caps, and file from a file part.
// The array is what the prompt's "图1"/"视频1"/"音频1" refer to. Because the
// platform refuses frames together with any other asset, and a lone image
// would otherwise have no single home, a declaration names either the frames
// pair or the reference and document channels; the same wire model is
// declared twice to serve both modes. A request carries a prompt, media, or
// both, and both surfaces document the model-chosen duration (smart
// duration) in place of a requested one. A media-array render comes back
// with an AAC soundtrack even when the request named no audio.
//   - kind "generate": native text generation on the classic text surface
//     or the multimodal one (generate.api), with tool calling, structured
//     output, the thinking controls, and the platform's hosted web search.
//     Media input rides the multimodal surface only. Reasoning traces come
//     back as reasoning parts; generate.replay_reasoning declares the
//     models whose input consumes reasoning_content again (multi-turn tool
//     calling on thinking models), and everywhere else a traced turn
//     reports the trace dropped rather than replaying one the platform
//     would refuse. Provider knobs no canonical field expresses — seed,
//     top_k, repetition_penalty, presence_penalty, stop,
//     parallel_tool_calls, the search switches, the thinking budget, the
//     VL high-resolution switch — ride the GenerateOptions extension, and
//     the sources a hosted search retrieved come back as the response's
//     web-search output.
//   - kind "embed": text embeddings on the text-embedding surface. The
//     canonical items lower to input.texts batches, one request per batch
//     (the model's cap is declared as embed.batch_max, 10 by default,
//     which every model version accepts), with the per-request token
//     counts summed into the response. Custom dimensions bind only to the
//     sizes the declaration lists — the platform takes an exact size, not
//     a bound — and the query/document role plus the retrieval instruction
//     ride the EmbedOptions extension.
//   - kind "tts": native speech synthesis on the Qwen-TTS shape of the
//     multimodal-generation surface, where the request carries input.text
//     and the required input.voice (plus input.language_type where the
//     declaration serves it) and the answer is one WAV file. The surface
//     has no format control, so tts.encoding, tts.sample_rate_hz, and
//     tts.channels declare the payload the model serves; the canonical
//     audio format requires an encoding, so the declaration states one and
//     a request naming another is refused rather than answered in a shape
//     the caller did not choose. The answer's part states that payload
//     encoding and names no media type — the canonical alphabet has no WAV
//     container token, and the file is a RIFF container around the declared
//     PCM — so a caller that needs the container reads its header. The
//     surface streams the same audio in SSE frames, which this driver does
//     not serve yet: a streaming request is refused at compile time.
//   - kind "transcribe": native file transcription on the asynchronous
//     recognition surface, which every non-realtime model family shares:
//     the request submits one audio URL (input.file_urls) and the answer is
//     the transcript the terminal query's result file carries. The audio
//     source has to be a URL — the platform fetches the file itself, so
//     inline bytes have nowhere to ride — and that result file is the one
//     artifact the driver downloads itself, because the transcript is the
//     answer rather than a pointer at one. The decoded answer carries the
//     joined text, the sentence and word timing the platform reports, and
//     the task's billed audio duration. The controls the families document —
//     the recognition context the canonical prompt lowers to, the language
//     hints, timestamp alignment, the filler-word and sensitive-word
//     filters, the hotword channels, the track selector, the dialect switch
//     — lower only where the declaration names them, and the sync flash
//     models plus the realtime WebSocket surface stay unserved: the kind
//     speaks the asynchronous file pipeline only, and a session request
//     stays unsupported.
//
// Every model declaration states what its surface accepts: the protocol
// (async pipeline or sync call), the endpoint family (text2image,
// image2image, video-generation, image2video), the allowed sizes or
// resolution tiers and durations, and the optional knobs. Undeclared facts
// stay unvalidated — the endpoint's own validation decides — while a
// declared fact is enforced before any request is sent, which is why a knob
// a model's declaration leaves off (a negative prompt on the media array,
// say) rejects the option instead of sending a field the endpoint never
// named.
//
// The media surfaces carry no channel for opaque caller metadata, so
// GenerateRequest.RequestMetadata is reported dropped on the compile report
// rather than silently left behind.
//
// # Models
//
// The driver ships no model line-up: every model is declared through the
// spec's models list — its kind, its family parameters (surface, batch
// caps, reasoning replay, the speech payload the canonical format has to
// match), and what it accepts and produces — and a kind the driver does
// not serve stays fail closed. The OpenAI-compatible
// endpoint stays available through the openai driver for deployments that
// prefer it; this driver serves the native surfaces, which are the only
// way to reach the media, embedding, speech, and recognition families and
// the only way to carry the platform's native generation controls.
//
// # Wiring
//
// Wire the factory into the deployment config by provider id:
//
//	builder := config.NewBuilder(
//		map[string]config.Factory{"dashscope": dashscope.Factory()},
//		resolvers,
//	)
//
// Each profile resolves one api_key secret; the spec optionally overrides
// base_url (the regional workspace host), sets workspace_id (sent as
// X-DashScope-WorkSpace, required for RAM sub-account keys), and paces task
// polling. A deployment that has verified that a set of models or
// credentials can verify each other's reasoning traces shares one
// reasoning_scope token across them; unset keeps the conservative
// per-address scope.
//
// # Retries
//
// The provider Spec accepts `http_retries` (total wire attempts including
// the first), mapped to the httpkit retry budget. Nil keeps the httpkit
// default; 0 disables provider-level retries so the route Router owns the
// budget. Retry-After and wire attempts are propagated onto ProviderFailure
// so Router backoff and trace wire_attempts can observe them.
package dashscope
