package runtime

import (
	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// Sandbox is the runtime's resolved view of the document's sandbox section: the
// values the composed chain was built from, handed to [WithSandboxDecorator] so a
// host that builds a second chain does not re-derive the document — and does not
// drift from it.
//
// It is the composed view, not [SandboxConfig], the block a host writes in the
// document: the fallbacks below are already applied.
type Sandbox struct {
	// Config is the validated document, for the backend blocks the resolved
	// fields below do not cover (seatbelt, bwrap, windows, remote). The runtime
	// keeps using it, so treat it as read-only.
	Config Config
	// WorkDir is the effective working directory: sandbox.workdir when set,
	// otherwise the first files root. It is where commands run, and the
	// directory a companion chain should be rooted at. Reading
	// Config.Sandbox.WorkDir directly is not the same thing: it is empty
	// whenever the deployment relied on the files root.
	WorkDir string
	// Net is the posture the document declares, for a companion chain to adopt
	// as its own.
	//
	// It is not inherited from the call: engine.WithDefaults takes Net from the
	// chain's defaults (defaults win entirely), so the value the exec tools pass
	// per call does not survive a retry chain unless that chain sets it.
	Net permission.NetPolicy
	// Defaults is exactly what the runtime merges into every call through
	// engine.WithDefaults: the timeout ceiling, the cpu and memory budgets, and
	// — on a backend that confines them — the configured writes.
	//
	// It shares its *permission.Policy, slices and maps with the composed chain,
	// so treat it as read-only: changing a field here rewrites the boundary the
	// confined chain enforces instead of your copy.
	Defaults sandbox.ExecOptions
}

// WithSandboxDecorator wraps the sandbox runner the runtime composed, after the
// backend and the document's policy defaults are in place and before the journal
// is opened. Whatever it returns is what exec_command, exec_session,
// [Runtime.Runner] and the journal all see, so host instrumentation and the
// tools cannot disagree about the process boundary.
//
// Ordering is deliberate: engine.WithDefaults is inside the decorator (it is
// already applied to the runner the decorator receives and is repeated in
// [Sandbox.Defaults]), so a host observes the deployment's effective timeout,
// budget and write posture rather than the caller's raw request.
//
// The decorator must return a non-nil runner. An error or a nil fails [New] and
// closes the runner it was given, like any other build step; a typed-nil value
// (a nil pointer widened into the interface) is not detected, so the decorator
// itself must not return one.
//
// Ownership stays with the runtime: it closes the returned value exactly once,
// and only that value. Anything the decorator creates — a retry chain, a metrics
// sink — is the decorator's to close, and a decorator that replaces rather than
// wraps the runner must close what it drops.
//
// A runner-level decorator sees sessions as well as one-shot commands. A
// decorator that needs the completed result of a command (escalation recognizes
// an OS refusal in it) implements [sandbox.Executor] on the runner it returns;
// engine.Exec is what dispatches to it.
func WithSandboxDecorator(fn func(Sandbox, sandbox.Runner) (sandbox.Runner, error)) Option {
	return func(o *options) { o.sandboxDecorator = fn }
}
