package provider

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunWithFallback_Success(t *testing.T) {
	circuit := NewCircuit(1)
	policy := DefaultFallbackPolicy()

	result, err := RunWithFallback(context.Background(), circuit, policy, "test.op",
		1,
		func(i int) string { return "primary" },
		func(ctx context.Context, i int) (string, error) {
			return "ok", nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok" {
		t.Fatalf("result = %q, want ok", result)
	}
}

func TestRunWithFallback_FallbackOnError(t *testing.T) {
	circuit := NewCircuit(2)
	policy := DefaultFallbackPolicy()

	result, err := RunWithFallback(context.Background(), circuit, policy, "test.op",
		2,
		func(i int) string { return []string{"primary", "fallback"}[i] },
		func(ctx context.Context, i int) (string, error) {
			if i == 0 {
				return "", errors.New("primary down")
			}
			return "from-fallback", nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "from-fallback" {
		t.Fatalf("result = %q, want from-fallback", result)
	}
}

func TestRunWithFallback_RetrySuccess(t *testing.T) {
	circuit := NewCircuit(1)
	policy := FallbackPolicy{MaxAttempts: 3}

	calls := 0
	result, err := RunWithFallback(context.Background(), circuit, policy, "test.op",
		1,
		func(i int) string { return "provider" },
		func(ctx context.Context, i int) (string, error) {
			calls++
			if calls < 3 {
				return "", errors.New("503 unavailable")
			}
			return "ok", nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok" {
		t.Fatalf("result = %q, want ok", result)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestRunWithFallback_SkipProvider(t *testing.T) {
	circuit := NewCircuit(3)
	policy := DefaultFallbackPolicy()

	result, err := RunWithFallback(context.Background(), circuit, policy, "test.op",
		3,
		func(i int) string { return []string{"a", "b", "c"}[i] },
		func(ctx context.Context, i int) (string, error) {
			if i < 2 {
				return "", ErrSkipProvider
			}
			return "from-c", nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "from-c" {
		t.Fatalf("result = %q, want from-c", result)
	}
}

func TestRunWithFallback_AllSkipped(t *testing.T) {
	circuit := NewCircuit(2)
	policy := DefaultFallbackPolicy()

	_, err := RunWithFallback(context.Background(), circuit, policy, "test.op",
		2,
		func(i int) string { return []string{"a", "b"}[i] },
		func(ctx context.Context, i int) (string, error) {
			return "", ErrSkipProvider
		},
	)
	if err == nil {
		t.Fatal("expected error when all providers skipped")
	}
}

func TestRunWithFallback_AllExhausted(t *testing.T) {
	circuit := NewCircuit(2)
	policy := DefaultFallbackPolicy()

	_, err := RunWithFallback(context.Background(), circuit, policy, "test.op",
		2,
		func(i int) string { return []string{"a", "b"}[i] },
		func(ctx context.Context, i int) (string, error) {
			return "", errors.New("fail")
		},
	)
	if err == nil {
		t.Fatal("expected error when all providers exhausted")
	}
}

func TestRunWithFallback_CircuitBreaker(t *testing.T) {
	circuit := NewCircuit(2)
	policy := FallbackPolicy{
		MaxAttempts:       1,
		CircuitBreakAfter: 1,
		CircuitOpen:       time.Minute,
	}

	names := []string{"primary", "fallback"}

	// First call: primary fails and opens circuit, fallback succeeds.
	_, err := RunWithFallback(context.Background(), circuit, policy, "test.op",
		2,
		func(i int) string { return names[i] },
		func(ctx context.Context, i int) (string, error) {
			if i == 0 {
				return "", errors.New("503 unavailable")
			}
			return "ok", nil
		},
	)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Second call: primary should be skipped (circuit open).
	primaryCalled := false
	_, err = RunWithFallback(context.Background(), circuit, policy, "test.op",
		2,
		func(i int) string { return names[i] },
		func(ctx context.Context, i int) (string, error) {
			if i == 0 {
				primaryCalled = true
			}
			return "ok", nil
		},
	)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if primaryCalled {
		t.Fatal("primary should have been skipped due to open circuit")
	}
}

func TestRunWithFallback_ObserverReport(t *testing.T) {
	circuit := NewCircuit(2)
	policy := DefaultFallbackPolicy()

	var got Report
	ctx := WithObserver(context.Background(), ObserverFunc(func(r Report) {
		got = r
	}))

	_, _ = RunWithFallback(ctx, circuit, policy, "test.op",
		2,
		func(i int) string { return []string{"primary", "fallback"}[i] },
		func(ctx context.Context, i int) (string, error) {
			if i == 0 {
				return "", errors.New("down")
			}
			return "ok", nil
		},
	)

	if got.Operation != "test.op" {
		t.Fatalf("Operation = %q, want test.op", got.Operation)
	}
	if got.SelectedProvider != "fallback" {
		t.Fatalf("SelectedProvider = %q, want fallback", got.SelectedProvider)
	}
	if !got.FallbackUsed {
		t.Fatal("FallbackUsed = false, want true")
	}
	if len(got.Attempts) != 2 {
		t.Fatalf("Attempts = %d, want 2", len(got.Attempts))
	}
	if got.Attempts[0].Outcome != "error" || got.Attempts[1].Outcome != "success" {
		t.Fatalf("unexpected attempt outcomes: %+v", got.Attempts)
	}
}

func TestRunWithFallback_NonFallbackableError(t *testing.T) {
	circuit := NewCircuit(2)
	policy := FallbackPolicy{
		MaxAttempts:    1,
		ShouldFallback: func(err error) bool { return false },
	}

	fallbackCalled := false
	_, err := RunWithFallback(context.Background(), circuit, policy, "test.op",
		2,
		func(i int) string { return []string{"a", "b"}[i] },
		func(ctx context.Context, i int) (string, error) {
			if i == 1 {
				fallbackCalled = true
			}
			return "", errors.New("fatal")
		},
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if fallbackCalled {
		t.Fatal("fallback should not be called for non-fallbackable error")
	}
}

func TestRunWithFallback_ContextCancelDuringBackoff(t *testing.T) {
	circuit := NewCircuit(1)
	policy := FallbackPolicy{
		MaxAttempts:  3,
		RetryBackoff: time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := RunWithFallback(ctx, circuit, policy, "test.op",
		1,
		func(i int) string { return "provider" },
		func(ctx context.Context, i int) (string, error) {
			return "", errors.New("503 unavailable")
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRunWithFallback_ContextCancelledBeforeStart(t *testing.T) {
	circuit := NewCircuit(2)
	policy := DefaultFallbackPolicy()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	_, err := RunWithFallback(ctx, circuit, policy, "test.op",
		2,
		func(i int) string { return "provider" },
		func(ctx context.Context, i int) (string, error) {
			called = true
			return "ok", nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("execFn should not be called when ctx is already cancelled")
	}
}

func TestRunWithFallback_ContextCancelDuringRetryNoBackoff(t *testing.T) {
	circuit := NewCircuit(1)
	// RetryBackoff==0 means the backoff sleep is not a cancellation checkpoint;
	// retries must still stop deterministically once ctx is cancelled.
	policy := FallbackPolicy{MaxAttempts: 5, RetryBackoff: 0}

	ctx, cancel := context.WithCancel(context.Background())

	calls := 0
	_, err := RunWithFallback(ctx, circuit, policy, "test.op",
		1,
		func(i int) string { return "provider" },
		func(ctx context.Context, i int) (string, error) {
			calls++
			cancel() // cancel while returning a retryable error that ignores ctx
			return "", errors.New("503 unavailable")
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (retries must stop after cancellation)", calls)
	}
}

func TestCircuit_NonRetryableFailuresTripBreaker(t *testing.T) {
	c := NewCircuit(1)
	policy := FallbackPolicy{CircuitBreakAfter: 2, CircuitOpen: time.Minute}
	base := time.Now()

	// A non-retryable but provider-attributable hard failure (e.g. an internal
	// provider error) must still trip the breaker after CircuitBreakAfter — the
	// breaker must not require retryable errors to open (#19).
	hardErr := &ProviderError{Code: "internal_error", Op: "op", Message: "boom", Retry: false, Fallback: true}
	if !IsProviderFault(hardErr) {
		t.Fatal("internal_error must be provider-attributable")
	}
	if c.OnFailure(0, base, policy, IsProviderFault(hardErr)) {
		t.Fatal("circuit should not open on the first hard failure")
	}
	if !c.OnFailure(0, base, policy, IsProviderFault(hardErr)) {
		t.Fatal("persistent hard (non-retryable) failures should open the breaker")
	}
	if c.Allow(0, base.Add(time.Second)) {
		t.Fatal("circuit should be open after consecutive hard failures")
	}
}

func TestCircuit_HalfOpenRecovery(t *testing.T) {
	c := NewCircuit(1)
	policy := FallbackPolicy{CircuitBreakAfter: 2, CircuitOpen: 50 * time.Millisecond}
	base := time.Now()

	c.OnFailure(0, base, policy, true)
	if !c.OnFailure(0, base, policy, true) {
		t.Fatal("circuit should open after 2 failures")
	}
	if c.Allow(0, base.Add(10*time.Millisecond)) {
		t.Fatal("circuit should stay open within the open window")
	}

	after := base.Add(60 * time.Millisecond)
	if !c.Allow(0, after) {
		t.Fatal("circuit should allow a probe (half-open) after the window elapses")
	}
	// A successful probe fully closes the circuit.
	c.OnSuccess(0)
	if !c.Allow(0, after) {
		t.Fatal("circuit should be closed after a successful probe")
	}
}

func TestCircuit_HalfOpenProbeFailureReopensFromCleanCount(t *testing.T) {
	c := NewCircuit(1)
	policy := FallbackPolicy{CircuitBreakAfter: 2, CircuitOpen: 50 * time.Millisecond}
	base := time.Now()

	c.OnFailure(0, base, policy, true)
	c.OnFailure(0, base, policy, true) // opens

	after := base.Add(60 * time.Millisecond)
	if !c.Allow(0, after) {
		t.Fatal("expected a half-open probe to be allowed")
	}
	// A single probe failure re-opens immediately from the decayed count,
	// rather than requiring an ever-growing streak.
	if !c.OnFailure(0, after, policy, true) {
		t.Fatal("half-open probe failure should re-open the circuit")
	}
	if c.Allow(0, after.Add(10*time.Millisecond)) {
		t.Fatal("circuit should be open again after the probe failure")
	}
	// And it remains bounded: the next window again yields a single probe.
	if !c.Allow(0, after.Add(60*time.Millisecond)) {
		t.Fatal("expected another probe after the second window elapses")
	}
}

func TestRunWithFallback_ZeroProviders(t *testing.T) {
	circuit := NewCircuit(0)
	policy := DefaultFallbackPolicy()

	result, err := RunWithFallback(context.Background(), circuit, policy, "test.op",
		0,
		func(i int) string { return "" },
		func(ctx context.Context, i int) (string, error) {
			t.Fatal("should not be called")
			return "", nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error for zero providers: %v", err)
	}
	if result != "" {
		t.Fatalf("result = %q, want empty", result)
	}
}

func TestSleepWithContext_Completes(t *testing.T) {
	ok := SleepWithContext(context.Background(), time.Millisecond)
	if !ok {
		t.Fatal("expected true")
	}
}

func TestSleepWithContext_ZeroDuration(t *testing.T) {
	ok := SleepWithContext(context.Background(), 0)
	if !ok {
		t.Fatal("expected true for zero duration")
	}
}

func TestSleepWithContext_Cancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ok := SleepWithContext(ctx, time.Second)
	if ok {
		t.Fatal("expected false for cancelled context")
	}
}

// TestCircuit_ClientFaultDoesNotTripBreaker locks in that client-attributable
// failures (bad input) never open a healthy provider's breaker, while
// provider-attributable faults still do (regression guard for the breaker
// accounting change).
func TestCircuit_ClientFaultDoesNotTripBreaker(t *testing.T) {
	c := NewCircuit(1)
	policy := DefaultFallbackPolicy() // CircuitBreakAfter = 2
	now := time.Now()

	badInput := &ProviderError{Code: "bad_audio", Op: "tts.synthesize", Message: "invalid sample rate", Retry: false, Fallback: true}
	for i := 0; i < 10; i++ {
		if c.OnFailure(0, now, policy, IsProviderFault(badInput)) {
			t.Fatalf("client-fault failure #%d wrongly opened the breaker", i+1)
		}
	}
	if !c.Allow(0, now) {
		t.Fatal("healthy provider should stay allowed after client-fault failures")
	}

	// Sanity: provider-attributable faults still trip after CircuitBreakAfter.
	provErr := &ProviderError{Code: "provider_unavailable", Op: "tts.synthesize", Message: "503", Retry: true, Fallback: true}
	_ = c.OnFailure(0, now, policy, IsProviderFault(provErr))
	if !c.OnFailure(0, now, policy, IsProviderFault(provErr)) {
		t.Fatal("provider-attributable faults should still trip the breaker")
	}
}

// TestRunWithFallback_ClientFaultDoesNotSkipHealthyProvider verifies that a
// client repeatedly sending bad input never causes a healthy provider to be
// skipped with an open circuit (the regression the diff review flagged).
func TestRunWithFallback_ClientFaultDoesNotSkipHealthyProvider(t *testing.T) {
	circuit := NewCircuit(1)
	policy := DefaultFallbackPolicy()

	calls := 0
	for req := 0; req < 5; req++ {
		_, err := RunWithFallback(context.Background(), circuit, policy, "tts.synthesize",
			1,
			func(i int) string { return "primary" },
			func(ctx context.Context, i int) (string, error) {
				calls++
				return "", &ProviderError{Code: "bad_audio", Op: "tts.synthesize", Message: "bad input", Retry: false, Fallback: true}
			},
		)
		if err == nil {
			t.Fatalf("req %d: expected an error for bad input", req)
		}
	}
	if calls != 5 {
		t.Fatalf("execFn called %d times, want 5 — a healthy provider was wrongly skipped by an open breaker on client-fault input", calls)
	}
}
