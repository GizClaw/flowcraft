// Package image is the MiniMax image-generation adapter. It targets
// MiniMax's POST /v1/image_generation endpoint
// (https://platform.minimax.io/docs/api-reference/image-generation-i2i)
// and exposes it through the standard sdk/llm.LLM interface so callers
// can route image generation through the same resolver, fallback, and
// credential-profile machinery as chat models.
//
// # Input mapping
//
// The adapter translates a [model.Message] list into a single
// MiniMax request:
//
//   - The last user message's text parts are concatenated into the prompt field (max 1500 chars per upstream).
//   - All [model.PartImage] parts across user messages are forwarded as subject_reference[].image_file URLs.
//   - System and assistant messages are dropped.
//
// When zero reference images are present the call degrades to
// text-to-image (still served by the same endpoint).
//
// # Image options
//
// Image-specific knobs (size, aspect ratio, n, seed, response
// format) flow through [llm.GenerateOptions.ImageGen]. The response
// is parsed into a single assistant [model.Message] whose Parts are
// [model.PartImage] entries (one per generated image).
//
// # Streaming
//
// The endpoint is synchronous; GenerateStream wraps Generate via
// [llm.NewOneChunkStream] so the [llm.StreamMessage] contract holds.
//
// # Authentication
//
// Bearer token in the Authorization header. The provider key is
// "minimax-image" so existing "minimax" chat credentials remain
// independent (deployments often use the same API key but different
// rate-limit pools).
package image
