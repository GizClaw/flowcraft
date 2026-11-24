package agent

import (
	"context"
	"errors"
	"sync"
)

// FailureKind classifies why a turn failed.
//
// It is a separate vocabulary from the tool layer's: "the tool returned an error" and
// "the model call failed" are different diagnoses, and collapsing them into one set
// makes "why did this turn die" answerable only by the last error written. The two
// travel in separate boxes for that reason.
//
// The set is closed because dashboards group by it; a failure nobody classified is
// reported as "unclassified", which is the signal that a stage is missing its call to
// [RecordFailure].
type FailureKind string

const (
	// FailureModel means the model call failed after its retries.
	FailureModel FailureKind = "model_error"
	// FailureTool means a tool failed and the turn could not continue.
	FailureTool FailureKind = "tool_error"
	// FailurePolicyDenied means a policy refused a tool or a model call.
	FailurePolicyDenied FailureKind = "policy_denied"
	// FailureTimeout means the turn exceeded its deadline.
	FailureTimeout FailureKind = "timeout"
	// FailureCanceled means the caller ended the turn: the run was stopped, which is
	// neither the driver's failure nor a deadline.
	FailureCanceled FailureKind = "canceled"
	// FailurePanicked means the driver, or something it called, panicked and was
	// recovered.
	FailurePanicked FailureKind = "panicked"
	// FailureUnclassified is what an observer reports for a failure no stage
	// classified.
	FailureUnclassified FailureKind = "unclassified"
)

// ClassifyFailure maps an error a stage returned onto the vocabulary. A driver that
// respects its context reports the deadline or cancellation it was given; that is not a
// driver bug.
func ClassifyFailure(err error) FailureKind {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return FailureTimeout
	case errors.Is(err, context.Canceled):
		return FailureCanceled
	default:
		return FailureUnclassified
	}
}

// The failure box is diagnostics, not authority: it travels on the context because
// every stage of a turn would otherwise have to thread it through. Authority travels in
// the request, as a parameter.

type failureBox struct {
	mu   sync.Mutex
	kind FailureKind
}

type failureKey struct{}

// WithFailureBox installs the per-turn classification box. The turn decorator does it
// once, before the driver runs, so a classification is possible whether or not
// instrumentation is installed; a context that already carries one is returned
// unchanged, so an inner stage never shadows the outer box.
func WithFailureBox(ctx context.Context) context.Context {
	if box, ok := ctx.Value(failureKey{}).(*failureBox); ok && box != nil {
		return ctx
	}
	return context.WithValue(ctx, failureKey{}, &failureBox{})
}

// RecordFailure classifies the current turn's failure.
//
// The first classification wins: the innermost stage is closest to the cause, and an
// outer timeout wrapping an inner policy denial must not rewrite the reason after the
// fact.
func RecordFailure(ctx context.Context, kind FailureKind) {
	if kind == "" {
		return
	}
	box, ok := ctx.Value(failureKey{}).(*failureBox)
	if !ok || box == nil {
		return
	}
	box.mu.Lock()
	if box.kind == "" {
		box.kind = kind
	}
	box.mu.Unlock()
}

// FailureKindFrom returns the classification recorded for ctx.
//
// ok is false when the context carries no box at all, which means the caller is outside a
// turn. A box that exists but classified nothing reports [FailureUnclassified]: an
// observer reading a failed turn's kind needs an answer, and "nothing was recorded" is a
// signal that a stage is missing its call to [RecordFailure], not a reason to leave the
// attribute empty.
func FailureKindFrom(ctx context.Context) (FailureKind, bool) {
	kind, ok := recordedKind(ctx)
	if !ok {
		return "", false
	}
	if kind == "" {
		return FailureUnclassified, true
	}
	return kind, true
}

// recordedKind is the raw value: empty when the box exists but nothing classified. It is
// what the recorder needs, since "unclassified" is a verdict rather than a default.
func recordedKind(ctx context.Context) (FailureKind, bool) {
	box, ok := ctx.Value(failureKey{}).(*failureBox)
	if !ok || box == nil {
		return "", false
	}
	box.mu.Lock()
	defer box.mu.Unlock()
	return box.kind, true
}
