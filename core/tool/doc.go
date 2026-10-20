// Package tool defines the tool contract and the dispatch around it. It
// holds no concrete tool: a deployment supplies those through [Source]
// implementations, and the runtime dispatches model-issued calls against
// them.
//
// # Roles
//
// A [Tool] is one callable capability: a [github.com/GizClaw/flowcraft/core/message.ToolDefinition]
// the model sees plus Execute, which runs one call. A [Source] contributes
// tools, eagerly ([Source.Tools]) or lazily ([Source.LazyTools], resolved
// only when the model actually asks for one). A [Catalog] is the read side
// a middleware or a prompt builder needs. [Assembly] wires sources into a
// catalog and closes them together; [Registry] is the mutable catalog an
// [Assembly] can add to.
//
// [Executor] is the one [Dispatcher] in this package: it composes the
// middleware chain over a bare catalog lookup. The chain is declared at
// construction and immutable afterwards, so an Executor is safe for
// concurrent use by construction.
//
// # Invariants
//
// Every failure — unknown tool, panicking tool, tool error — surfaces as a
// [github.com/GizClaw/flowcraft/core/message.ToolResult] with IsError set.
// Dispatch never returns a Go error, because a model-issued call is data,
// not a control-flow event: the result goes back into the conversation
// either way. [Dispatcher.ExecuteAll] preserves input order in its output
// slice, so a caller may zip calls and results.
//
// The dynamic-exposure policy ([Policy], [Budget]) is the other half of the
// catalog contract: it decides which definitions reach the model in one
// turn, per name and in aggregate, and [Session] is the per-turn view that
// tracks required, discovered and called names.
//
// # Boundary
//
// MCP-backed tools live in tool/mcp; the built-in cross-cutting middleware
// lives in tool/middleware. Neither is imported here. Tools reach host
// capabilities (workspace, sandbox, session identity) through the context
// helpers in this package, never by holding a host object.
package tool
