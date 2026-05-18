package background

import (
	"context"
	"errors"
)

// Outcome is the normalized result label for a background work item.
type Outcome string

const (
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeCanceled  Outcome = "canceled"
	OutcomeTimeout   Outcome = "timeout"
	OutcomeFailed    Outcome = "failed"
)

// String returns the stable telemetry label for o.
func (o Outcome) String() string { return string(o) }

// Classify returns the normalized outcome for err.
func Classify(err error) Outcome {
	switch {
	case err == nil:
		return OutcomeSucceeded
	case errors.Is(err, context.DeadlineExceeded):
		return OutcomeTimeout
	case errors.Is(err, context.Canceled):
		return OutcomeCanceled
	default:
		return OutcomeFailed
	}
}
