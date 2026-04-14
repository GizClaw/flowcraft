package provider

import (
	"context"
	"errors"
	"testing"
)

func TestProviderError_Error(t *testing.T) {
	err := &ProviderError{
		Code:    "timeout",
		Op:      "tts.synthesize",
		Message: "request timed out",
	}
	want := "tts.synthesize: request timed out"
	if got := err.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestProviderError_ErrorWithCause(t *testing.T) {
	cause := errors.New("connection reset")
	err := &ProviderError{
		Code:    "transport_error",
		Op:      "stt.recognize",
		Message: "stream broken",
		Cause:   cause,
	}
	want := "stt.recognize: stream broken: connection reset"
	if got := err.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestProviderError_Unwrap(t *testing.T) {
	cause := errors.New("underlying")
	err := &ProviderError{Cause: cause}
	if !errors.Is(err, cause) {
		t.Fatal("expected errors.Is to find cause")
	}
}

func TestProviderError_ImplementsClassifiedError(t *testing.T) {
	err := &ProviderError{
		Code:     "timeout",
		Op:       "test",
		Message:  "timed out",
		Retry:    true,
		Fallback: true,
	}
	var ce ClassifiedError
	if !errors.As(err, &ce) {
		t.Fatal("ProviderError should implement ClassifiedError")
	}
	if ce.ErrorCode() != "timeout" {
		t.Fatalf("ErrorCode() = %q, want timeout", ce.ErrorCode())
	}
	if !ce.IsRetryable() {
		t.Fatal("IsRetryable() = false, want true")
	}
	if !ce.IsFallbackable() {
		t.Fatal("IsFallbackable() = false, want true")
	}
}

func TestProviderError_NonRetryable(t *testing.T) {
	err := &ProviderError{
		Code:     "bad_audio",
		Op:       "stt.recognize",
		Message:  "invalid codec",
		Retry:    false,
		Fallback: false,
	}
	var ce ClassifiedError
	if !errors.As(err, &ce) {
		t.Fatal("ProviderError should implement ClassifiedError")
	}
	if ce.IsRetryable() {
		t.Fatal("IsRetryable() = true, want false")
	}
	if ce.IsFallbackable() {
		t.Fatal("IsFallbackable() = true, want false")
	}
}

func TestClassifyByMessage(t *testing.T) {
	tests := []struct {
		msg  string
		want string
	}{
		{"request timeout", "timeout"},
		{"deadline exceeded", "timeout"},
		{"connection refused", "transport_error"},
		{"broken pipe", "transport_error"},
		{"closed pipe error", "transport_error"},
		{"unexpected eof", "transport_error"},
		{"websocket closed", "transport_error"},
		{"transport error", "transport_error"},
		{"service unavailable", "provider_unavailable"},
		{"HTTP 503", "provider_unavailable"},
		{"HTTP 502", "provider_unavailable"},
		{"HTTP 429", "provider_unavailable"},
		{"rate limit exceeded", "provider_unavailable"},
		{"invalid audio format", "bad_audio"},
		{"unsupported codec", "bad_audio"},
		{"wrong sample rate", "bad_audio"},
		{"something else happened", "internal_error"},
	}
	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			if got := ClassifyByMessage(tt.msg); got != tt.want {
				t.Fatalf("ClassifyByMessage(%q) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}

func TestIsRetryable_ClassifiedError(t *testing.T) {
	retryable := &ProviderError{Code: "timeout", Retry: true, Fallback: true, Op: "test", Message: "t"}
	if !IsRetryable(retryable) {
		t.Fatal("expected retryable for ClassifiedError with Retry=true")
	}

	nonRetryable := &ProviderError{Code: "bad_audio", Retry: false, Fallback: false, Op: "test", Message: "t"}
	if IsRetryable(nonRetryable) {
		t.Fatal("expected non-retryable for ClassifiedError with Retry=false")
	}
}

func TestIsRetryable_StandardErrors(t *testing.T) {
	if !IsRetryable(context.DeadlineExceeded) {
		t.Fatal("expected context.DeadlineExceeded to be retryable")
	}
	if IsRetryable(context.Canceled) {
		t.Fatal("expected context.Canceled to be non-retryable")
	}
	if IsRetryable(nil) {
		t.Fatal("expected nil to be non-retryable")
	}
}

func TestIsRetryable_UntypedErrors(t *testing.T) {
	if !IsRetryable(errors.New("connection timeout")) {
		t.Fatal("expected 'connection timeout' to be retryable")
	}
	if !IsRetryable(errors.New("service unavailable")) {
		t.Fatal("expected 'service unavailable' to be retryable")
	}
	if IsRetryable(errors.New("invalid argument")) {
		t.Fatal("expected 'invalid argument' to be non-retryable")
	}
}

func TestCanFallback_ClassifiedError(t *testing.T) {
	fallbackable := &ProviderError{Code: "timeout", Fallback: true, Op: "test", Message: "t"}
	if !CanFallback(fallbackable) {
		t.Fatal("expected fallbackable for ClassifiedError with Fallback=true")
	}

	nonFallbackable := &ProviderError{Code: "bad_audio", Fallback: false, Op: "test", Message: "t"}
	if CanFallback(nonFallbackable) {
		t.Fatal("expected non-fallbackable for ClassifiedError with Fallback=false")
	}
}

func TestCanFallback_StandardErrors(t *testing.T) {
	if CanFallback(context.Canceled) {
		t.Fatal("expected context.Canceled to be non-fallbackable")
	}
	if !CanFallback(errors.New("some error")) {
		t.Fatal("expected generic error to be fallbackable")
	}
	if CanFallback(nil) {
		t.Fatal("expected nil to be non-fallbackable")
	}
}
