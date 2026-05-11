package script

import (
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ErrorKind enumerates the errdefs categories scripts may set on
// signal.error({ kind, message }). Only categories that can plausibly
// originate from script logic are exposed — auth, rate-limit, timeout
// and similar wire-level classifications stay in the Go layer where
// they are produced.
//
// Unknown kinds passed by a script degrade to [ErrorKindInternal] in
// [SignalToError] so a typo in script land cannot escape as an
// unclassified error.
type ErrorKind string

const (
	// ErrorKindValidation marks user-input / config validation
	// failures. Maps to [errdefs.Validation] (HTTP 400).
	ErrorKindValidation ErrorKind = "validation"

	// ErrorKindNotFound marks a lookup the script performed that
	// returned nothing. Maps to [errdefs.NotFound] (HTTP 404).
	ErrorKindNotFound ErrorKind = "not_found"

	// ErrorKindBudgetExceeded marks a per-run / per-tenant budget
	// (token, cost, iteration count) the script self-detected.
	// Maps to [errdefs.BudgetExceeded] (HTTP 429).
	ErrorKindBudgetExceeded ErrorKind = "budget_exceeded"

	// ErrorKindPolicyDenied marks a policy-layer refusal raised from
	// inside the script (tool allow-list, role check, …). Maps to
	// [errdefs.PolicyDenied] (HTTP 403).
	ErrorKindPolicyDenied ErrorKind = "policy_denied"

	// ErrorKindNotAvailable marks a transient unavailability of a
	// dependency the script needed (e.g. a knowledge store down).
	// Maps to [errdefs.NotAvailable] (HTTP 503).
	ErrorKindNotAvailable ErrorKind = "not_available"

	// ErrorKindInternal is the default fallback for uncategorised
	// script-side errors. Maps to [errdefs.Internal] (HTTP 500).
	ErrorKindInternal ErrorKind = "internal"
)

// SignalToError maps a script control signal into a Go error that
// fulfils the appropriate errdefs / engine classification, so host
// code (scriptnode, future scriptengine, …) can share one consistent
// translation step instead of branching on sig.Type by hand.
//
// Mapping rules:
//
//   - nil signal, Type "done", or unrecognised Type return nil.
//   - Type "error": [errdefs] wrapper chosen by Kind; unknown / empty
//     Kind degrades to [errdefs.Internal] (the value is preserved in
//     the wrapped message so observability surfaces the typo).
//   - Type "interrupt": [engine.Interrupted] with Cause taken from
//     Kind (unknown values fall through to [engine.CauseCustom]) and
//     Detail taken from Message.
//
// Returning nil for an unknown Type matches the existing scriptnode
// behaviour — only "error" / "interrupt" are observable side-effects.
func SignalToError(sig *Signal) error {
	if sig == nil {
		return nil
	}
	switch sig.Type {
	case "", "done":
		return nil
	case "error":
		return signalErrorToErrdefs(sig)
	case "interrupt":
		return engine.Interrupted(engine.Interrupt{
			Cause:  signalKindToCause(sig.Kind),
			Detail: sig.Message,
		})
	default:
		return nil
	}
}

func signalErrorToErrdefs(sig *Signal) error {
	msg := sig.Message
	if msg == "" {
		msg = "script error"
	}
	base := errors.New(msg)
	switch ErrorKind(sig.Kind) {
	case ErrorKindValidation:
		return errdefs.Validation(base)
	case ErrorKindNotFound:
		return errdefs.NotFound(base)
	case ErrorKindBudgetExceeded:
		return errdefs.BudgetExceeded(base)
	case ErrorKindPolicyDenied:
		return errdefs.PolicyDenied(base)
	case ErrorKindNotAvailable:
		return errdefs.NotAvailable(base)
	case ErrorKindInternal, "":
		return errdefs.Internal(base)
	default:
		// Unknown kind: preserve the raw value in the chain for
		// observability, but classification stays internal so we
		// never lie about category.
		return errdefs.Internal(fmt.Errorf("kind=%q: %w", sig.Kind, base))
	}
}

func signalKindToCause(kind string) engine.Cause {
	switch engine.Cause(kind) {
	case engine.CauseUserCancel,
		engine.CauseUserInput,
		engine.CauseHostShutdown,
		engine.CauseCustom:
		return engine.Cause(kind)
	}
	return engine.CauseCustom
}
