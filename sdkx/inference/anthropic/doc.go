// Package anthropic provides the Anthropic (Claude) inference provider.
//
// The provider serves the Messages API surface only, which is the whole
// Claude API: chat generation, unary and SSE streamed. Claude has no
// embeddings, image generation, speech, or transcription endpoints, so
// those operations are absent rather than stubbed.
//
// The generate driver binds through the package kernel (kernel.go):
// sibling Claude platforms — AWS Bedrock, Google Vertex AI — serve the
// same Messages protocol behind their own authentication and model-id
// schemes, and reuse KernelGenerate with their own clients and capability
// declarations instead of forking the driver.
//
// Notable protocol mappings:
//
//   - system and developer messages compile into the request's system
//     blocks, which is the only system channel the API has.
//   - Tool results compile into user messages carrying tool_result blocks;
//     consecutive user messages merge, because the API rejects consecutive
//     same-role messages.
//   - The canonical reasoning switch emits thinking: {type: "disabled"}
//     or adaptive thinking directly; the reasoning effort knob maps to
//     output_config.effort (low/medium/high, matching the canonical enum
//     exactly). The kernel can switch the control to the binary-thinking
//     dialect (thinking: {type: "adaptive"}, no effort levels) for
//     Messages-compatible platforms such as MiniMax.
//   - Thinking and redacted thinking blocks decode into canonical
//     reasoning parts — the signature (or redacted data) rides the part's
//     Signature slot — and hoist ahead of other blocks when context
//     compiles back, which the API requires for round-trips. Unsigned
//     reasoning cannot verify on round-trip, so it compiles as Dropped
//     with the reason on the ledger.
//   - max_tokens is required by the API; when the request leaves
//     MaxOutputTokens unset, the compiler pins the catalog default
//     (DefaultMaxTokens) rather than inventing a per-model figure.
//
// # Credentials
//
// Each profile carries exactly one secret:
//
//	api_key (required): the Anthropic API key, sent as x-api-key.
//
// # Deployment wiring
//
// The provider Spec carries an optional base_url override (for gateways)
// and optional model declarations for deployments serving unreleased or
// gateway-renamed models. All models are generate kind; the built-in
// catalog tracks the Claude lineup of July 2026.
//
// # Retries
//
// The provider Spec accepts `http_retries` (total wire attempts including
// the first). Nil keeps the anthropic-sdk-go default (two retries); 0
// disables SDK-internal retries so the route Router owns the budget; N maps
// to option.WithMaxRetries(N-1). Retry-After and the SDK retry count are
// propagated onto ProviderFailure so Router backoff and trace
// wire_attempts can observe them.
package anthropic
