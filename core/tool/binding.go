package tool

import (
	"context"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
)

// Binding is everything one tool call is bound to: the session it serves, the
// policy that session froze, the tool projection it reads and the capabilities it
// may reach.
//
// It is a parameter rather than a field on the tool, and nothing about it is read
// from the context, for the same two reasons: a tool that held a session's file
// boundary could not be shared across sessions, and one that read its authority from
// ambient state could be handed authority its caller never granted. Receiving it per
// call makes both impossible.
//
// The name is deliberate. The request — what the model asked for — is
// [message.ToolCall]; this is the authority that request runs under, narrowed per
// tool by dispatch. It is fixed for the life of a session: a grant is frozen when
// the session is bound, and a capability set is assembled then, so nothing here
// changes between two calls of the same session.
//
// The zero value is not "unrestricted": [Binding.Valid] is false and dispatch refuses
// it. A binding is only ever assembled by the host.
type Binding struct {
	// Grant is the authority the session was opened with. It is a value, not a gate:
	// a tool reads it to give a correctable
	// error ("this path is outside the session's read roots") instead of failing
	// opaquely.
	Grant policy.Grant
	// Session is the conversation the call belongs to.
	Session Session
	// View is the session's projection of the tool table: what may be shown.
	View View
	// Caps is the session's capability pocket, narrowed by dispatch to the needs of
	// the tool being called. A capability the tool did not declare reads as absent.
	Caps CapView
}

// Bind assembles the authority for one session.
//
// It is the host's entry point and the only way to reach a valid Binding: the two
// fields no call can do without are the two arguments. The view and the capability
// pocket follow with [Binding.WithView] and [Binding.WithCaps].
func Bind(session Session, grant policy.Grant) Binding {
	return Binding{Session: session, Grant: grant}
}

// WithView returns the binding carrying the session's tool projection.
func (b Binding) WithView(v View) Binding {
	b.View = v
	return b
}

// WithCaps returns the binding carrying the session's capability pocket.
func (b Binding) WithCaps(set *CapSet) Binding {
	b.Caps = set.View()
	return b
}

// WithCapsView returns the binding carrying an already-built capability view, for a
// host that narrowed the pocket further before binding.
func (b Binding) WithCapsView(v CapView) Binding {
	b.Caps = v
	return b
}

// Valid reports whether this binding was assembled by a host.
func (b Binding) Valid() bool { return b.Session != nil }

// Narrow returns the binding restricted to allow: the needs one tool declared.
//
// Dispatch calls it before every execution, so a tool receives only the capabilities
// it asked for even though the session holds more. Narrowing to a precomputed
// allowance allocates nothing.
func (b Binding) Narrow(allow Allow) Binding {
	b.Caps = b.Caps.With(allow)
	return b
}

// Session is the conversation view a tool gets: which conversation it serves, and
// the scratch space it may keep state in. Nothing else.
type Session interface {
	// ID is the conversation's address.
	ID() string
	// Scratch returns the session-scoped store, already partitioned for this tool
	// by dispatch, so two tools cannot collide on a key.
	Scratch() Scratch
}

// Scratch is per-session storage a tool owns.
//
// It is untyped because a typed slot per tool would make every tool a package of its
// own. Values live exactly as long as the session: the session table drops the whole
// map when the conversation is evicted, so nothing here needs a sweeper.
type Scratch interface {
	// Load returns the value stored under key.
	Load(key string) (any, bool)
	// Store keeps value under key, replacing what was there.
	Store(key string, value any)
	// Delete removes key.
	Delete(key string)
}

// View is the session's projection of the tool table.
//
// It is defined here, at the consumer, so the projection package can implement it
// without this package depending on that one.
type View interface {
	// Definitions is what the model may see this round.
	Definitions() []message.ToolDefinition
	// Visible reports whether name may be shown or called.
	Visible(name string) bool
	// RecordCall tells the projection the model used name.
	RecordCall(call message.ToolCall)
}

// ViewFactory builds one session's view over a deployment's tool table.
//
// The split is the reason this type exists. What may be shown at all is a property of the
// grant, which is per deployment; what *this conversation* has seen, what it discovered, and
// how much of its per-round budget it has spent is a property of the conversation. A single
// view shared by two sessions would let one conversation's tool calls evict the other's, and
// would make the prompt a session sends depend on what somebody else did — which is neither
// reproducible nor isolated.
//
// So a deployment holds one factory (the policy, and whatever index it shares) and every
// session asks it for its own view.
type ViewFactory func() View

// Dispatcher is the execution contract consumers depend on.
type Dispatcher interface {
	// Execute runs one call under the binding.
	Execute(ctx context.Context, b Binding, request message.ToolCall) message.ToolResult
	// ExecuteAll runs calls concurrently, preserving order.
	ExecuteAll(ctx context.Context, b Binding, requests []message.ToolCall) []message.ToolResult
}

// Dispatch is the function form of "execute one tool call under a binding".
// Middleware operates on this signature.
type Dispatch func(ctx context.Context, b Binding, request message.ToolCall) message.ToolResult

// Middleware wraps a Dispatch. mws[0] is outermost (sees the call first and the
// result last).
type Middleware func(next Dispatch) Dispatch
