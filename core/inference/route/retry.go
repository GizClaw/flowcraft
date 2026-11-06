package route

import (
	"context"
	"errors"
	"math/rand"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
)

// RetryDecision says what a failure means for the dispatcher.
type RetryDecision int

const (
	// RetryStop ends the call: the request or the caller is the problem, and
	// another target would fail the same way.
	RetryStop RetryDecision = iota
	// RetrySameTarget means the failure is plausibly transient; the same target
	// gets another attempt before the dispatcher moves on.
	RetrySameTarget
	// RetryNextTarget means this target cannot serve the call — not now and
	// probably not ever for this request — so the next candidate is tried
	// immediately.
	RetryNextTarget
)

// RetryPolicy bounds one dispatch. Zero values mean "one attempt per target,
// every target once, no delay": a dispatcher that does not wait unless asked.
type RetryPolicy struct {
	// AttemptsPerTarget is how many times one target is tried for a transient
	// failure. Zero means one.
	AttemptsPerTarget int
	// MaxTargets caps how many candidates are tried. Zero means every candidate
	// the selector returned.
	MaxTargets int
	// BaseDelay is the first backoff delay; it doubles per attempt. Zero means
	// no waiting.
	BaseDelay time.Duration
	// MaxDelay caps the backoff. Zero means no cap.
	MaxDelay time.Duration
}

// effective resolves the zero values against a candidate count.
func (p RetryPolicy) effective(targets int) RetryPolicy {
	if p.AttemptsPerTarget <= 0 {
		p.AttemptsPerTarget = 1
	}
	if p.MaxTargets <= 0 || p.MaxTargets > targets {
		p.MaxTargets = targets
	}
	return p
}

// backoff is the delay before attempt n of one target (n starts at 1).
func (p RetryPolicy) backoff(attempt int) time.Duration {
	if p.BaseDelay <= 0 {
		return 0
	}
	delay := p.BaseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if p.MaxDelay > 0 && delay >= p.MaxDelay {
			return p.MaxDelay
		}
	}
	if p.MaxDelay > 0 && delay > p.MaxDelay {
		return p.MaxDelay
	}
	// Jitter keeps a fleet of callers from retrying in lockstep.
	return delay/2 + time.Duration(rand.Int63n(int64(delay/2)+1))
}

// classify maps an inference failure onto a retry decision.
//
// The categories are the contract's, not the provider's: a driver classifies
// what failed, and this decides whether another attempt or another target can
// plausibly answer.
func classify(err error) RetryDecision {
	switch {
	case err == nil:
		return RetryStop
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return RetryStop
	case inference.IsKind(err, inference.OperationInterrupted):
		return RetryStop
	// The target cannot serve this request: another candidate may.
	case inference.IsKind(err, inference.UnknownModel),
		inference.IsKind(err, inference.UnsupportedOperation),
		inference.IsKind(err, inference.UnsupportedFeature):
		return RetryNextTarget
	// The request itself is wrong, or the caller's policy refuses it. Repeating
	// it anywhere produces the same rejection.
	case inference.IsKind(err, inference.InvalidRequest),
		inference.IsKind(err, inference.InvalidExtension),
		inference.IsKind(err, inference.PolicyDenied),
		inference.IsKind(err, inference.UndefinedTool),
		inference.IsKind(err, inference.CompilerContractViolation):
		return RetryStop
	// A provider or its answer failed: transient, or a broken target worth
	// skipping.
	case inference.IsKind(err, inference.ProviderFailure),
		inference.IsKind(err, inference.ProviderTruncated):
		return RetrySameTarget
	default:
		return RetryNextTarget
	}
}

// sleep waits, or returns early when the call is cancelled.
func sleep(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
