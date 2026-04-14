package provider

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrSkipProvider is returned by an execute function to indicate that the
// current provider does not support the requested operation and should be
// skipped without recording a failure (e.g. provider does not implement
// a streaming interface).
var ErrSkipProvider = errors.New("provider: skip")

// SleepWithContext blocks for duration d or until ctx is cancelled.
func SleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// RunWithFallback executes an operation against multiple providers with
// retry, circuit-breaker, and fallback support.
//
// Parameters:
//   - n: number of providers
//   - nameFn: returns display name for provider at index i
//   - execFn: performs the operation; return ErrSkipProvider to skip silently
func RunWithFallback[T any](
	ctx context.Context,
	circuit *Circuit,
	policy FallbackPolicy,
	operation string,
	n int,
	nameFn func(int) string,
	execFn func(ctx context.Context, idx int) (T, error),
) (T, error) {
	policy = policy.Normalize()
	report := Report{Operation: operation}
	emit := func() {
		if observer := ObserverFromContext(ctx); observer != nil {
			observer.OnProviderReport(report)
		}
	}

	var zero T
	var lastErr error
	now := time.Now()

	for i := 0; i < n; i++ {
		name := nameFn(i)
		if !circuit.Allow(i, now) {
			report.Attempts = append(report.Attempts, Attempt{
				Provider: name,
				Attempt:  0,
				Outcome:  "skipped_open_circuit",
			})
			continue
		}

		skipped := false
		for attempt := 0; attempt < policy.MaxAttempts; attempt++ {
			result, err := execFn(ctx, i)
			if err == nil {
				circuit.OnSuccess(i)
				report.SelectedProvider = name
				report.FallbackUsed = i > 0
				report.Attempts = append(report.Attempts, Attempt{
					Provider: name,
					Attempt:  attempt + 1,
					Outcome:  "success",
				})
				emit()
				return result, nil
			}

			if errors.Is(err, ErrSkipProvider) {
				skipped = true
				break
			}

			lastErr = err
			retryable := policy.ShouldRetry(err)
			fallbackable := policy.ShouldFallback(err)
			circuitOpened := circuit.OnFailure(i, time.Now(), policy, retryable)
			report.Attempts = append(report.Attempts, Attempt{
				Provider:      name,
				Attempt:       attempt + 1,
				Outcome:       "error",
				Retryable:     retryable,
				Fallbackable:  fallbackable,
				CircuitOpened: circuitOpened,
				Error:         err.Error(),
			})

			if retryable && attempt+1 < policy.MaxAttempts {
				if !SleepWithContext(ctx, policy.RetryBackoff) {
					report.Error = ctx.Err().Error()
					emit()
					return zero, ctx.Err()
				}
				continue
			}
			if !fallbackable {
				report.Error = err.Error()
				emit()
				return zero, err
			}
			break
		}
		if skipped {
			continue
		}
	}

	if lastErr == nil && n > 0 {
		lastErr = fmt.Errorf("%s: all providers exhausted", operation)
	}
	if lastErr != nil {
		report.Error = lastErr.Error()
	}
	emit()
	return zero, lastErr
}
