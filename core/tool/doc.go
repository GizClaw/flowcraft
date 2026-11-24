// Package tool defines the tool contract and the authority one call runs under.
// It holds no concrete tool: a deployment supplies those through [Source]
// implementations, and the runtime dispatches model-issued calls against them.
//
// # Roles
//
// A [Tool] is one callable capability: a [github.com/GizClaw/flowcraft/core/message.ToolDefinition]
// the model sees, the [Need]s it declares, and Execute, which runs one call. A
// [Source] contributes tools, eagerly ([Source.Tools]) or lazily
// ([Source.LazyTools], resolved only when the model actually asks for one). A
// [Catalog] is the read side a middleware or a prompt builder needs; [Registry] is
// the mutable catalog a deployment builds.
//
// [Binding] is everything one call is bound to: the session, the policy it froze,
// the [View] it may show, and the capability pocket it may reach. Dispatch lives in
// tool/dispatch and narrows a binding per tool before calling it. A tool that runs
// another tool declares [NestedSlot] and calls the [Nested] it receives, so a nested
// call inherits its caller's session rather than reaching for a process-wide table.
//
// # Capabilities
//
// A tool reads a capability through a [Slot]: the package that owns the capability
// declares the slot, the host fills it in a [CapSet], and the tool reads it out of
// the [CapView] it was bound to. Declaring one is a local act, so the contract does
// not grow a field per capability. A slot the tool did not declare reads as the zero
// value, which is how "a tool cannot be given authority it did not ask for" is
// enforced without copying or nil-ing fields.
//
// # Invariants
//
// Every failure — unknown tool, panicking tool, tool error — surfaces as a
// [github.com/GizClaw/flowcraft/core/message.ToolResult] with IsError set. Dispatch
// never returns a Go error, because a model-issued call is data, not a control-flow
// event: the result goes back into the conversation either way.
// [Dispatcher.ExecuteAll] preserves input order in its output slice, so a caller may
// zip calls and results.
//
// The zero [Binding] is not "unrestricted": [Binding.Valid] is false and dispatch
// refuses it. A binding is only ever assembled by the host.
//
// # Boundary
//
// MCP-backed tools live in tool-mcp; the built-in cross-cutting middleware lives in
// tool/middleware; the discovery projection lives in tool/projection. None is
// imported here. A tool reaches host capabilities through its [Binding], never by
// holding a host object and never by reading ambient state from the context.
package tool
