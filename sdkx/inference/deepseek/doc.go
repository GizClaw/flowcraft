// Package deepseek implements the DeepSeek provider for the unified
// inference runtime.
//
// # Protocol
//
// DeepSeek serves an OpenAI-compatible chat completions API
// (https://api.deepseek.com) — there is no Responses API surface, so the
// openai provider's drivers do not apply here. The transport rides the
// official openai-go chat completions client pointed at the DeepSeek base
// URL; DeepSeek-owned request fields (thinking, reasoning_effort) go out
// as per-request JSON overrides, and the reasoning_content response extra
// reads back through the SDK's raw JSON metadata. (DeepSeek also serves an
// Anthropic-compatible endpoint, but its thinking blocks are signatureless
// and mandatory after tool calls — the opposite of the anthropic driver's
// signature-verified round-trip policy — so this package speaks chat
// completions instead of reusing that kernel.)
//
// # Catalog
//
// The built-in catalog covers the V4 series — deepseek-v4-flash and
// deepseek-v4-pro, both hybrid thinking models with a 1M token context.
// The legacy deepseek-chat / deepseek-reasoner aliases retired on
// 2026-07-24 and are deliberately absent. Custom models declare through
// the spec's models list; unknown channels stay fail closed.
//
// # Behavior mapping
//
//   - Thinking mode is API-default enabled: the compiler sends no thinking
//     field unless the canonical reasoning switch sets one explicitly
//     (reasoning_enabled: false emits thinking disabled; the request
//     validator already forbids disabling alongside an effort).
//   - The reasoning effort knob passes through verbatim as
//     reasoning_effort; DeepSeek maps low/medium to high and xhigh to max
//     on its side, so the wire value stays the canonical one.
//   - reasoning_content decodes into canonical reasoning parts. The
//     round-trip follows DeepSeek's own rule: an assistant turn that
//     performed tool calls must carry its reasoning back (the compiler
//     attaches it natively, and a tool-calling turn without a trace still
//     carries the field, empty, because the API 400s otherwise); a turn
//     without tool calls has no channel for reasoning, so it compiles as
//     Dropped with the reason on the ledger.
//   - While thinking runs, temperature and top_p have no effect on
//     DeepSeek's side; they compile natively (the API accepts them), which
//     the response surface cannot distinguish from honored.
//   - frequency/presence penalties are deprecated provider-wide and have
//     no canonical surface here at all.
//   - JSON output supports response_format json_object only;
//     schema-constrained output is rejected at compile time.
//   - The finish reason insufficient_system_resource classifies as a
//     retryable provider failure, not a terminal finish.
//   - Usage accounts prompt cache hits (prompt_cache_hit_tokens) as input
//     cache reads and reasoning_tokens as output reasoning, included in
//     the output total.
//
// # Wiring
//
// Wire the factory into the deployment config by provider id:
//
//	builder := config.NewBuilder(
//		map[string]config.Factory{"deepseek": deepseek.Factory()},
//		resolvers,
//	)
//
// Each profile resolves one api_key secret; the spec optionally overrides
// base_url and declares extra models.
//
// # Retries
//
// The provider Spec accepts `http_retries` (total wire attempts including
// the first). Nil keeps the openai-go default (two retries); 0 disables
// SDK-internal retries so the route Router owns the budget; N maps to
// option.WithMaxRetries(N-1). Retry-After and the SDK retry count are
// propagated onto ProviderFailure so Router backoff and trace
// wire_attempts can observe them.
package deepseek
