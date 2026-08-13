// Package anthropic provides the Anthropic inference provider.
//
// The provider serves Generate through the Messages API (unary + stream):
// text, vision input, tool calling, reasoning with signatures, and
// JSON-schema output. The provider owns its kernel (compiler, transports,
// and decoders) and registers itself as an inference.Provider resource.
package anthropic
