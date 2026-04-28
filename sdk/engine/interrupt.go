package engine

// Cause classifies why a run was asked to stop. The agent layer maps
// these onto its higher-level commit/discard policy (e.g. discard the
// partial output on user_input, commit it on host_shutdown).
//
// Engines should NEVER branch on Cause for control flow — Cause is
// metadata for the host, not a directive for the engine. The engine's
// only correct response to any cause is "stop cleanly and return".
type Cause string

const (
	// CauseUnknown is the zero value. Hosts should avoid sending it;
	// it exists so a zero-value Interrupt is recognisable.
	CauseUnknown Cause = ""

	// CauseUserCancel is a user-initiated cancel ("stop talking",
	// "abort this turn"). Output is typically discarded.
	CauseUserCancel Cause = "user_cancel"

	// CauseUserInput is a barge-in: the user spoke / typed and the
	// agent should yield to fresh input. Output is typically discarded.
	CauseUserInput Cause = "user_input"

	// CauseHostShutdown is a graceful shutdown from the host.
	// Output should typically be committed if any was produced.
	CauseHostShutdown Cause = "host_shutdown"

	// CauseCustom carries a host-defined cause in [Interrupt.Detail].
	CauseCustom Cause = "custom"
)

// Interrupt is the value the host sends through Host.Interrupts() to
// ask the running engine to stop. It is also the payload of the
// [Interrupted] error so the host can introspect why.
//
// Interrupt is a plain value — copy it freely.
type Interrupt struct {
	Cause  Cause
	Detail string
}

// Interrupted wraps an [Interrupt] as an error that satisfies
// [errdefs.IsInterrupted]. The recommended usage from an engine is:
//
//	case intr := <-h.Interrupts():
//	    return engine.Interrupted(intr)
//
// Hosts inspecting the result use the standard errdefs / errors.As
// idiom:
//
//	if errdefs.IsInterrupted(err) {
//	    var ie engine.InterruptedError
//	    if errors.As(err, &ie) {
//	        switch ie.Cause {
//	        case engine.CauseUserInput: ...
//	        }
//	    }
//	}
//
// A zero-value Interrupt still produces a well-formed error so
// callers don't need to special-case CauseUnknown.
func Interrupted(intr Interrupt) error {
	return interruptedErr{Interrupt: intr}
}

// InterruptedError is the concrete error type returned by [Interrupted].
// It is exported so hosts can use [errors.As] to destructure it; the
// preferred way to produce one is [Interrupted], not direct
// construction.
//
// Implements [errdefs] interrupted-marker so [errdefs.IsInterrupted]
// returns true on any error wrapping (or equal to) one of these.
type InterruptedError = interruptedErr

// interruptedErr is unexported because hosts should construct via
// Interrupted(...) and inspect via errors.As (using the exported alias
// InterruptedError). Keeping the underlying type unexported prevents
// foreign packages from synthesising one without the constructor.
type interruptedErr struct {
	Interrupt
}

// Error formats the cause and detail into a human-readable message.
func (e interruptedErr) Error() string {
	switch {
	case e.Cause == CauseUnknown && e.Detail == "":
		return "engine: interrupted"
	case e.Cause == CauseUnknown:
		return "engine: interrupted: " + e.Detail
	case e.Detail == "":
		return "engine: interrupted (" + string(e.Cause) + ")"
	default:
		return "engine: interrupted (" + string(e.Cause) + "): " + e.Detail
	}
}

// Interrupted is the errdefs marker method, NOT a name accessor. It
// makes errdefs.IsInterrupted(err) report true for any error wrapping
// this type.
func (interruptedErr) Interrupted() {}

// Compile-time assertion that interruptedErr satisfies the errdefs
// interrupted marker (interface{ Interrupted() }). errdefs.IsInterrupted
// uses errors.As against this exact shape, so this assertion guarantees
// classification works.
var _ interface{ Interrupted() } = interruptedErr{}
