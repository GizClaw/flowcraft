package errdefs

import (
	"errors"
	"testing"
	"time"
)

func TestWithRetryAfterAndRetryAfter(t *testing.T) {
	err := WithRetryAfter(NotAvailable(errors.New("boom")), 3*time.Second)
	got, ok := RetryAfter(err)
	if !ok || got != 3*time.Second {
		t.Fatalf("RetryAfter = %v/%v, want 3s/true", got, ok)
	}
}

func TestRetryAfterWalksWrappedChain(t *testing.T) {
	inner := WithRetryAfter(Timeout(errors.New("slow")), 2*time.Second)
	wrapped := errors.Join(errors.New("outer"), inner)
	got, ok := RetryAfter(wrapped)
	if !ok || got != 2*time.Second {
		t.Fatalf("RetryAfter = %v/%v, want 2s/true", got, ok)
	}
}

func TestRetryAfterIgnoresMissingAndNonPositive(t *testing.T) {
	if _, ok := RetryAfter(errors.New("plain")); ok {
		t.Fatal("plain error reported a retry hint")
	}
	if got := WithRetryAfter(errors.New("plain"), 0); got == nil {
		t.Fatal("WithRetryAfter with zero returned nil")
	}
	if _, ok := RetryAfter(WithRetryAfter(errors.New("plain"), 0)); ok {
		t.Fatal("non-positive hint reported")
	}
}

func TestParseRetryAfter(t *testing.T) {
	for value, want := range map[string]time.Duration{
		"":    0,
		"1":   time.Second,
		" 5 ": 5 * time.Second,
		"-1":  0,
		"abc": 0,
		"1.5": 0, // seconds form only
	} {
		if got := ParseRetryAfter(value); got != want {
			t.Errorf("ParseRetryAfter(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestWithRetryCountAndRetryCount(t *testing.T) {
	err := WithRetryCount(NotAvailable(errors.New("boom")), 3)
	if got := RetryCount(err); got != 3 {
		t.Fatalf("RetryCount = %d, want 3", got)
	}
	if got := RetryCount(errors.New("plain")); got != 0 {
		t.Fatalf("RetryCount(plain) = %d, want 0", got)
	}
}

func TestParseRetryCount(t *testing.T) {
	for value, want := range map[string]int{
		"":    0,
		"0":   0,
		"2":   2,
		" 3 ": 3,
		"-1":  0,
		"x":   0,
	} {
		if got := ParseRetryCount(value); got != want {
			t.Errorf("ParseRetryCount(%q) = %d, want %d", value, got, want)
		}
	}
}
