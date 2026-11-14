// Package anthropic provides the Anthropic inference provider.
//
// The provider serves Generate through the Messages API: text, vision input,
// tool calling, and reasoning with signatures. Unary and streaming calls share
// one canonical request and one canonical response; only the transport differs.
//
// Usage normalization follows the wire: Anthropic's input_tokens counts only the
// tokens that were neither read from nor written to the cache, so the canonical
// InputTokens is the inclusive prompt total and the cache buckets are subsets.
package anthropic
