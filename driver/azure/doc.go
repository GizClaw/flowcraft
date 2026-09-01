// Package azure provides the Azure OpenAI inference provider.
//
// The provider serves the operation surfaces Azure OpenAI shares with the
// OpenAI wire protocol through a self-hosted kernel:
//
//   - Generate: Responses API over chat deployments, unary + SSE stream.
//   - Embed: embeddings deployments.
//   - Image: gpt-image generation and image-to-image edits, unary + SSE
//     stream with optional partial-image previews; inline reference images
//     route to images/edits, and the image_options extension carries a mask
//     for local inpainting plus the partial_images preview count.
//   - Audio generation: speech deployments, unary + raw byte stream.
//
// Transcription is intentionally absent: core/inference does not expose the
// transcription operation surface yet.
//
// Deployments are the catalog: Azure routes by deployment name, so every
// model in spec.models declares one deployment plus the operation kind and
// optional capability flags (including effort_none for reasoning
// deployments that accept effort "none" to disable thinking). Credentials
// live per profile under api_key; the resource endpoint and api-version
// live on the provider Spec.
package azure
