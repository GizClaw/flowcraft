package script

import (
	"context"
)

// Backend is one script execution engine.
//
// An implementation must be safe for concurrent use: two nodes of one step may
// invoke the same backend at the same time, and a host builds one backend for the
// process rather than one per node. A backend that needs a pool or a process owns
// that machinery itself; the node never sees it.
type Backend interface {
	// Name is the value written in config.backend.
	Name() string
	// Capabilities is what this backend promises. The node refuses a
	// configuration that asks for more.
	Capabilities() Capabilities
	// Compile validates one source and returns something [Backend.Run] can
	// execute. The result may be cached by the node, keyed by the source
	// version, so it must be immutable, or internally synchronised.
	//
	// A compile failure is an authoring mistake and belongs to
	// [errdefs.Validation]: it is the backend's job to classify it.
	Compile(ctx context.Context, source Script) (Program, error)
	// Run executes one compiled program against a projected state and returns
	// the state it wants written.
	//
	// The state value domain matches encoding/json: nil, bool, float64, string,
	// []any and map[string]any, with one exception: a session is projected as
	// [Messages], which carries the sequence itself. A backend builds whatever it
	// wants from Messages.Slice; the intermediate map-per-message is not paid.
	// Run must not return a key the caller did not ask for; the node validates
	// the result either way.
	//
	// A budget that runs out is [errdefs.BudgetExceeded], and a script that
	// fails while running is [errdefs.Aborted]. A backend that returns an
	// unclassified error gets the node's default classification, which is
	// Aborted.
	Run(ctx context.Context, program Program, state map[string]any, budget Budget, host Host) (map[string]any, error)
}

// Program is one compiled source. The shape is the backend's business: a
// compiled interpreter program, a handle to a warm worker, a pool reference.
type Program interface {
	// Version is the source version this program was compiled from. The node
	// keeps the cache key, so a program that cannot report it is a backend bug.
	Version() string
}

// ToolResult is what one tool call answered.
//
// The text is the tool's content, not a structured value: a tool already speaks
// JSON when it has structure to convey, and re-shaping it here would invent a
// second schema.
type ToolResult struct {
	OK   bool
	Text string
}

// Host is the only way a backend reaches the host.
//
// Keeping the callback here, instead of letting a backend call the runtime
// directly, is what makes capability, recording and replay policy one
// implementation rather than one per language.
type Host interface {
	// Log reports one line from the script. A host routes it to its logger; the
	// node itself writes nothing, because a library that logs on its own account
	// cannot be silenced.
	Log(level LogLevel, msg string)
	// Tool runs one host tool and returns its result.
	//
	// The node enforces config.allow, records the call and classifies a failure,
	// so a backend calls this without knowing the policy. An error means the
	// script must stop: Starlark has no exceptions to catch, and a rule that
	// cannot complete its action should fail the step rather than guess.
	Tool(ctx context.Context, name string, args map[string]any) (ToolResult, error)
}

// LogLevel is how loud one script line is.
type LogLevel int

const (
	// LogDebug is a script's own trace line.
	LogDebug LogLevel = iota
	// LogInfo is a script's deliberate note.
	LogInfo
	// LogWarn is a script reporting something wrong it survived.
	LogWarn
)

// String renders the level for a log line.
func (l LogLevel) String() string {
	switch l {
	case LogDebug:
		return "debug"
	case LogInfo:
		return "info"
	case LogWarn:
		return "warn"
	}
	return "unknown"
}
