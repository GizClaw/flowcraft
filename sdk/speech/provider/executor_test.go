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
