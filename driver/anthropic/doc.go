// Package anthropic provides the Anthropic inference provider.
//
// The provider serves Generate through the Messages API (unary + stream):
// text, vision input, tool calling, reasoning with signatures, and
// JSON-schema output. The provider owns its kernel (compiler, transports,
// and decoders) and registers itself as an inference.Provider resource.
//
// Thinking blocks round-trip only for the model and account that signed them,
// so every trace the decoder produces carries the deployment's verification
// scope and the compiler replays a stored trace only when that scope matches;
// `wire.reasoning_scope` declares a shared scope for a deployment whose
// models or credentials are known to accept each other's traces.
package anthropic
