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
// It reports true if the full duration elapsed and false if ctx was cancelled.
// For d<=0 it still consults ctx so a cancelled context is honored immediately.
func SleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
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

	for i := 0; i < n; i++ {
		// Fail fast if the context is already done: return the context error
		// directly so it is never misclassified as a provider failure that
		// could trip the breaker.
		if err := ctx.Err(); err != nil {
			report.Error = err.Error()
			emit()
			return zero, err
		}

		// Recompute the clock per iteration: a slow multi-provider run can let
		// a provider's open window elapse mid-loop, so a single pre-loop
		// timestamp would keep skipping providers that are actually recovered.
		now := time.Now()
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
			// Guard each attempt on the context so a cancelled/expired ctx
			// stops retries deterministically even when RetryBackoff==0 (the
			// backoff sleep is otherwise the only cancellation checkpoint).
			if err := ctx.Err(); err != nil {
				report.Error = err.Error()
				emit()
				return zero, err
			}
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
			// Only provider-attributable faults count toward the breaker;
			// client-fault errors (bad input, cancellation) must not open a
			// healthy provider.
			circuitOpened := circuit.OnFailure(i, time.Now(), policy, IsProviderFault(err))
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
