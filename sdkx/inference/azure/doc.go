// Package azure provides the Azure OpenAI inference provider.
//
// The provider serves the operation surfaces Azure OpenAI shares with the
// OpenAI wire protocol, bound through the openai package's Kernel* helpers:
//
//   - Generate: Responses API over chat deployments, unary + SSE stream.
//   - Embed: embeddings deployments.
//   - Image: gpt-image generation deployments, unary only.
//   - Audio generation: speech deployments, unary + raw byte stream.
//   - Transcription: transcription deployments, unary only.
//
// Realtime stays absent for the same reason as the openai provider: the
// pinned SDK has no WebSocket coverage.
//
// # Deployments are the catalog
//
// Azure routes requests by deployment name, and the backing model is a
// management-plane property the API does not expose. There is therefore no
// built-in catalog: every model in spec.models declares one deployment name
// plus the operation kind it serves, and optional capability flags
// (vision / reasoning / dimensions) opt the deployment into channels the
// bare text surface rejects. Capabilities default off so a deployment never
// silently accepts a feature its backing model may not serve.
//
// # Credentials
//
// Each profile carries exactly one secret:
//
//	api_key (required): the Azure OpenAI resource key.
//
// The resource endpoint and api-version live on the provider Spec
// (endpoint, api_version), not per profile: all profiles of one provider
// config share one resource.
package azure
