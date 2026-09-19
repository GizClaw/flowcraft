// Package anthropic provides the Anthropic inference provider.
//
// The provider serves Generate through the Messages API (unary + stream):
// text, vision input, tool calling, reasoning with signatures, and
// JSON-schema output. The provider owns its kernel (compiler, transports,
// and decoders) and registers itself as an inference.Provider resource.
//
// Usage normalization: the wire's input_tokens counts only the tokens that
// were neither read from nor written to the cache, so the canonical
// inference.Usage.InputTokens this driver reports is the inclusive prompt
// total (input_tokens + cache_read_input_tokens +
// cache_creation_input_tokens) and TotalTokens follows it. Hosts that
// reconstructed the prompt size as InputTokens + cache read + cache write
// — the arithmetic of the raw wire counters — must use InputTokens (or
// TotalTokens - OutputTokens) alone; adding the cache buckets on top now
// double counts.
//
// Thinking blocks round-trip only for the model and account that signed them,
// so every trace the decoder produces carries the deployment's verification
// scope and the compiler replays a stored trace only when that scope matches;
// `wire.reasoning_scope` declares a shared scope for a deployment whose
// models or credentials are known to accept each other's traces.
package anthropic
