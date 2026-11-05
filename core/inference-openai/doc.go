// Package openai speaks the OpenAI wire protocol.
//
// # Why this is in core
//
// The OpenAI chat interface is not one vendor's interface. Deployments in this
// repository already point the OpenAI wire at DeepSeek and at Zhipu GLM
// (base_url + impl: openai), and the same shape covers Qwen, Moonshot, vLLM and
// most self-hosted servers. It is a protocol family, so the framework ships it
// rather than making every application find a driver before it can call a model.
//
// It ships without a vendor SDK on purpose. The official client hides the wire,
// and the wire is exactly what a driver must be able to test: the compiler
// produces a request the conformance suite can compare byte for byte against
// expected JSON, with no network involved.
//
// # What it covers
//
//	generate (unary)   chat completions, text and tool calls
//	generate (stream)  chat completions with server-sent events
//
// Deployment knobs — sampling, output limits — are [Settings] fields, not
// request fields, because an application decides them once. A request carries
// only the turn, what to produce, and which tools the model may call.
package openai
