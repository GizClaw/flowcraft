// Package message owns the provider-neutral wire DTOs shared by every chat
// turn: a role-tagged [Message] carrying an ordered [Content] of [Part]s,
// plus the tool-call DTOs ([ToolDefinition], [ToolCall], [ToolResult]) and the JSON
// Schema helpers that build a [ToolDefinition].
//
// The package has no runtime: no providers, no executors, no middleware.
// It is the canonical shape of "what travels on the wire" — the layers
// above it operate on these types but do not own them: core/inference for
// model calls, core/tool for tool dispatch, and core/agent for the
// transcript a task is checkpointed as. Two consumers can therefore share a
// message log without pulling in each other's runtime.
//
// The wire format is JSON. [Content.MarshalJSON] / [Content.UnmarshalJSON]
// tag each part with its [PartKind] so the same payload survives a
// round-trip through any tool that ignores FlowCraft's Go types.
//
// Tool results follow one representation rule: a JSON object payload is
// a structured [DataPart] built by [NewJSONContent], never stringified
// prose, while text and non-object payloads (arrays, scalars) stay
// [TextPart]s — [DataPart] holds objects only. One shape means
// redaction, result limiting, and driver lowering treat every tool's
// structured output the same way.
//
// The media subpackage holds the operation-neutral Source / Format types
// that ride inside multimodal Parts (images, audio, video). They are DTOs
// for the same reason and live here for the same reason.
package message
