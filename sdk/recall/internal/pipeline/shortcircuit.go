package pipeline

// ShortCircuit is the sentinel error a Stage returns to terminate
// the pipeline successfully without invoking any later stage or
// compensator. Reason is surfaced in the emitted StageDiagnostic's
// Err field so observers can attribute the early exit; it is not
// an error message in the failure sense.
//
// The pipeline framework treats ShortCircuit as a normal terminal
// outcome:
//
//   - Pipeline.Run returns nil to the caller.
//   - The short-circuiting stage's diagnostic carries
//     Status=short_circuit (Reason → Err).
//   - Later stages do not run and no diagnostic is emitted for
//     them.
//   - Compensators do NOT fire. Short-circuit is by definition a
//     non-failure outcome — only true errors trigger rollback.
//
// Callers detect ShortCircuit via errors.As since the framework
// matches it that way:
//
//	var sc pipeline.ShortCircuit
//	if errors.As(err, &sc) { ... }
type ShortCircuit struct {
	Reason string
}

// Error implements the error interface. The string contains the
// Reason so logs that swallow ShortCircuit through a generic
// err.Error() call still carry the attribution.
func (sc ShortCircuit) Error() string {
	if sc.Reason == "" {
		return "pipeline short-circuit"
	}
	return "pipeline short-circuit: " + sc.Reason
}

// ShortCircuitWith builds a ShortCircuit sentinel carrying the
// supplied reason. Stage authors return the value (NOT a pointer)
// from Run so the framework's errors.As targets the value receiver.
func ShortCircuitWith(reason string) error {
	return ShortCircuit{Reason: reason}
}

// IsShortCircuit reports whether err is or wraps a ShortCircuit
// sentinel. It is the convenience alternative to errors.As for
// callers that only need a yes/no answer.
func IsShortCircuit(err error) bool {
	if err == nil {
		return false
	}
	for cur := err; cur != nil; {
		if _, ok := cur.(ShortCircuit); ok {
			return true
		}
		u, ok := cur.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		cur = u.Unwrap()
	}
	return false
}
