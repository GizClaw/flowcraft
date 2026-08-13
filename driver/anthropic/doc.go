// Package anthropic provides the Anthropic inference provider.
//
// The provider serves Generate through the Messages API (unary + stream):
// text, vision input, tool calling, reasoning with signatures, and
// JSON-schema output. The kernel is shared with Claude-compatible
// platforms through exported Kernel helpers; the anthropic provider
// registers itself as an inference.Provider resource.
package anthropic
