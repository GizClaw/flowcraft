package errdefs

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// RetryAfterCoder is implemented by errors that carry a server-provided
// retry hint (typically from a Retry-After response header).
type RetryAfterCoder interface {
	RetryAfter() time.Duration
}

// RetryCountCoder is implemented by errors that carry the number of wire
// attempts (HTTP sends) that already happened before the failure surfaced.
type RetryCountCoder interface {
	RetryCount() int
}

type retryAfterHint struct {
	error
	retryAfter time.Duration
}

func (e retryAfterHint) Unwrap() error { return e.error }

func (e retryAfterHint) RetryAfter() time.Duration { return e.retryAfter }

// WithRetryAfter wraps err with a Retry-After hint. Nil and non-positive
// durations return err unchanged so callers can attach hints unconditionally.
func WithRetryAfter(err error, d time.Duration) error {
	if err == nil || d <= 0 {
		return err
	}
	return retryAfterHint{error: err, retryAfter: d}
}

// RetryAfter reads the hint from an error chain. It reports false when the
// chain carries no hint or the hint is not positive.
func RetryAfter(err error) (time.Duration, bool) {
	var coder RetryAfterCoder
	if errors.As(err, &coder) {
		d := coder.RetryAfter()
		return d, d > 0
	}
	return 0, false
}

// ParseRetryAfter parses the seconds form of a Retry-After header value.
// Invalid, empty, and negative values return zero.
func ParseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// ParseRetryCount parses a zero-based retry-count header (e.g. the
// X-Stainless-Retry-Count header vendor SDKs send). Invalid, empty, and
// negative values return zero.
func ParseRetryCount(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	count, err := strconv.Atoi(value)
	if err != nil || count < 0 {
		return 0
	}
	return count
}

type retryCountHint struct {
	error
	retryCount int
}

func (e retryCountHint) Unwrap() error { return e.error }

func (e retryCountHint) RetryCount() int { return e.retryCount }

// WithRetryCount wraps err with the number of wire attempts already made.
// Nil and non-positive counts return err unchanged.
func WithRetryCount(err error, count int) error {
	if err == nil || count <= 0 {
		return err
	}
	return retryCountHint{error: err, retryCount: count}
}

// RetryCount reads the wire-attempt count from an error chain. Zero means no
// count was recorded.
func RetryCount(err error) int {
	var coder RetryCountCoder
	if errors.As(err, &coder) {
		return coder.RetryCount()
	}
	return 0
}
