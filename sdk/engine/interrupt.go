package engine

import (
	"context"
	"sync"
)

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

// MergeInterrupts fans-in N independent interrupt channels into a
// single output channel — the natural shape sandbox / pod hosts need
// to combine "user cancel", "SIGTERM", "budget exceeded", "graceful
// stop", … into the one channel an Engine reads from
// [Host.Interrupts].
//
// Behaviour:
//
//   - The returned channel is closed once ctx is cancelled OR every
//     non-nil source channel has been closed. Engines reading from it
//     therefore see EOF when "everyone is done", matching how a single
//     cooperative-interrupt channel behaves today.
//   - Nil source channels are skipped silently — matches the documented
//     "nil means never fires" semantic in [Interrupter.Interrupts] and
//     keeps callers from having to filter their slice.
//   - When zero non-nil sources are supplied the returned channel is
//     a never-fires channel that is closed when ctx is cancelled. This
//     keeps the helper total — engines selecting on it stay correct
//     even in the trivial case.
//   - Order of forwarded interrupts is the natural runtime arrival
//     order across the source channels. No de-duplication: a host that
//     fires "shutdown" through two distinct sources will surface both.
//
// Forwarding goroutines exit promptly when ctx is cancelled, so the
// helper is safe to use in long-lived hosts that get re-created across
// reloads.
func MergeInterrupts(ctx context.Context, sources ...<-chan Interrupt) <-chan Interrupt {
	out := make(chan Interrupt)

	// Filter nil sources up front so the WaitGroup count matches the
	// number of goroutines actually launched. With ctx cancellation we
	// also need a separate sentinel to wake a parked send when ctx
	// fires before any source produces; a single fan-out goroutine
	// owns that responsibility.
	var live []<-chan Interrupt
	for _, ch := range sources {
		if ch != nil {
			live = append(live, ch)
		}
	}

	var wg sync.WaitGroup
	wg.Add(len(live))
	for _, ch := range live {
		go func(in <-chan Interrupt) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case intr, ok := <-in:
					if !ok {
						return
					}
					select {
					case <-ctx.Done():
						return
					case out <- intr:
					}
				}
			}
		}(ch)
	}

	// Closer: every forwarder exits on ctx.Done OR source close, then
	// wg.Wait unblocks and we close out exactly once with no pending
	// senders. The zero-source case still works: wg starts at 0 and
	// out closes as soon as ctx is cancelled (because the goroutine
	// blocks on wg.Wait, which returns immediately, so we then need a
	// ctx select to keep the channel alive until cancellation). Handle
	// that case explicitly so a 0-source merge doesn't return an
	// already-closed channel.
	go func() {
		if len(live) == 0 {
			<-ctx.Done()
			close(out)
			return
		}
		wg.Wait()
		close(out)
	}()

	return out
}
