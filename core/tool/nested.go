package tool

import (
	"context"

	"github.com/GizClaw/flowcraft/core/message"
)

// Nested dispatches another tool call under the same binding as the caller.
//
// It is the capability a tool declares when it needs to run a tool itself — a
// delegating tool that hands work to a sub-agent, a composite tool that fans out. The
// object a host provides is bound to one session: it already holds that session's
// [Binding], so a nested call cannot lose the session, the grant or the capability
// pocket, and there is no binding argument to forget to pass.
//
// # Why not a table, and why not a method on [View]
//
// A process-wide table of tools is the wrong shape twice. It exposes every definition
// the process carries, including the ones this session cannot see, so a tool holding it
// could enumerate its way past the projection; and it hands out execution without the
// session it belongs to, which is exactly the ambient authority [Binding] exists to
// remove.
//
// Putting Dispatch on [View] trades one problem for another. A [View] answers "what may
// be shown", and a nested dispatch needs the binding, which the view does not have;
// closing that loop means the projection must own a dispatcher, turning a read-side
// description of the toolset into an execution path.
//
// What remains is one session-bound object behind a slot, which is what this interface
// is. Visibility still resolves through the same binding, so a nested call sees exactly
// the toolset its caller does.
type Nested interface {
	// Dispatch runs one call under the caller's binding and reports the result the
	// way the model would see it.
	Dispatch(ctx context.Context, request message.ToolCall) message.ToolResult
}

// NestedSlot is the capability a tool declares in [Tool.Needs] to receive a [Nested].
//
// It is declared here, in the contract, because the host that provides it and the tools
// that consume it share no other package.
var NestedSlot = NewSlot[Nested]("tool.nested")
