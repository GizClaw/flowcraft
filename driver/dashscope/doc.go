// Package dashscope implements the Alibaba Cloud Model Studio (百炼 /
// DashScope) provider for the unified inference runtime, serving the Wan
// (万相) media families over their native protocol.
//
// # Protocol
//
// The driver speaks the native DashScope protocol, not the OpenAI or
// Anthropic compatibility surfaces: media models on this platform are
// native-only, and the compatibility endpoints cannot express the Wan
// surfaces at all. Requests post to
//
//	{base}/api/v1/services/aigc/...
//
// with Bearer auth and, on the task surfaces, the mandatory
// X-DashScope-Async: enable header. Two protocol shapes exist:
//
//   - The async task pipeline. Image generation and every video surface
//     create a task (the create response carries output.task_id), and the
//     caller polls GET {base}/api/v1/tasks/{task_id} until
//     output.task_status reaches SUCCEEDED. Task data — including the
//     generated links — is retained for 24 hours and then cleared, so the
//     response carries the provider URL verbatim and downloading it is the
//     caller's job.
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
// The driver serves two kinds:
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
// spec's models list, and a kind the driver does not serve stays fail
// closed. Text generation and embeddings are not served here — route those
// through the openai driver's compatibility endpoint, which the platform
// documents as the migration path for chat models.
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
// polling.
//
// # Retries
//
// The provider Spec accepts `http_retries` (total wire attempts including
// the first), mapped to the httpkit retry budget. Nil keeps the httpkit
// default; 0 disables provider-level retries so the route Router owns the
// budget. Retry-After and wire attempts are propagated onto ProviderFailure
// so Router backoff and trace wire_attempts can observe them.
package dashscope
