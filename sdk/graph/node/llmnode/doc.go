// Package llmnode implements the Go-native "llm" graph node and exposes
// a Register helper for binding it into a node.Factory.
//
// The node calls an LLM (resolved through llm.LLMResolver) and routes
// optional tool calls through a tool.Registry. Streaming events
// (token / tool_call / tool_result) are emitted via the
// graph.StreamPublisher handed in on graph.ExecutionContext.Publisher.
package llmnode
