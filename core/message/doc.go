// Package message owns the provider-neutral wire DTOs shared by every chat
// turn: a role-tagged [Message] carrying an ordered [Content] of [Part]s,
// plus the tool-call DTOs ([ToolDefinition], [ToolCall], [ToolResult]) and the JSON
// Schema helpers that build a [ToolDefinition].
//
// The package has no runtime: no providers, no executors, no middleware.
// It is the canonical shape of "what travels on the wire" — the layer
// above this package (core/inference for LLM calls, core/tool for tool
// execution, core/memory for stored turns) operates on these types but does
// not own them. Two consumers can therefore share a message log without
// pulling in each other's runtime.
//
// The wire format is JSON. [Content.MarshalJSON] / [Content.UnmarshalJSON]
// tag each part with its [PartKind] so the same payload survives a
// round-trip through any tool that ignores FlowCraft's Go types.
//
// The media subpackage holds the operation-neutral Source / Format types
// that ride inside multimodal Parts (images, audio, video). They are DTOs
// for the same reason and live here for the same reason.
package message
