// Package azure provides the Azure OpenAI inference provider.
//
// The provider serves the operation surfaces Azure OpenAI shares with the
// OpenAI wire protocol, bound through the driver/openai kernel helpers:
//
//   - Generate: Responses API over chat deployments, unary + SSE stream.
//   - Embed: embeddings deployments.
//   - Image: gpt-image generation deployments, unary only.
//   - Audio generation: speech deployments, unary + raw byte stream.
//
// Transcription is intentionally absent: core/inference does not expose the
// transcription operation surface yet.
//
// Deployments are the catalog: Azure routes by deployment name, so every
// model in spec.models declares one deployment plus the operation kind and
// optional capability flags. Credentials live per profile under api_key;
// the resource endpoint and api-version live on the provider Spec.
package azure
