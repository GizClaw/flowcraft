// Package deepseek implements the DeepSeek provider for the unified
// inference runtime.
//
// The provider speaks two generate surfaces:
//
//   - Chat Completions (`api: chat`, the default): text, tool calling,
//     json_object output, thinking control, reasoning_effort, and the
//     DeepSeek reasoning_content round-trip that thinking-mode tool loops
//     require. json_schema output is not available on this surface. The
//     deepseek-v4-flash-vision-exp model additionally accepts image input
//     (URL or base64) in user messages on this surface.
//   - Responses (`api: responses`): OpenAI-compatible Responses API on
//     https://api.deepseek.com/responses. deepseek-v4-flash,
//     deepseek-v4-pro, and deepseek-v4-flash-vision-exp support it. The
//     surface adds json_schema output, hosted web_search, and plain-text
//     reasoning item round-trips. The vision model carries input_image
//     parts in user messages here too. `include` is intentionally never
//     sent: DeepSeek does not support it.
//
// Credentials come exclusively from resource profiles: `api_key`
// authenticates every surface, and secret values may reference the
// environment with ${env:NAME}. The provider Spec redirects transport
// (base_url), selects the generate surface (api), bounds SDK retries
// (http_retries), and declares or overrides models (models).
//
// Deployments wire this package as an inference.Provider resource:
//
//	reg.MustRegister(deepseek.Factory())
//
//	resources:
//	  provider.deepseek: {kind: inference.Provider, impl: deepseek, settings: {...}}
//	  infer: {kind: inference.Assembly, impl: unified, deps: {provider.deepseek: provider.deepseek}}
package deepseek
