// Package httpkit carries the HTTP conveniences the hand-rolled provider
// clients (qwen, minimax, kimi) gave up when they stepped off vendor SDKs:
// a tuned connection-pooling transport and a bounded retry RoundTripper
// for transient failures.
package httpkit

import (
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// NewTransport returns a connection-pooling base transport tuned for a
// small number of provider hosts: many idle keep-alive connections per
// host so concurrent streams do not redial.
func NewTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 128
	transport.MaxIdleConnsPerHost = 32
	transport.IdleConnTimeout = 90 * time.Second
	return transport
}

// RetryConfig bounds the retry behaviour.
type RetryConfig struct {
	// MaxAttempts is the total number of tries including the first.
	MaxAttempts int
	// BaseDelay seeds the exponential backoff.
	BaseDelay time.Duration
	// MaxDelay caps one backoff sleep (a Retry-After hint may exceed it).
	MaxDelay time.Duration
}

// DefaultRetry retries transient failures twice after the first attempt
// with a 200ms-seeded exponential backoff.
var DefaultRetry = RetryConfig{
	MaxAttempts: 3,
	BaseDelay:   200 * time.Millisecond,
	MaxDelay:    2 * time.Second,
}

// NewRetryTransport decorates base with bounded retries for transient
// failures: network errors, 408, 429, and 5xx responses. The wrapper only
// engages while a request is replayable (net/http sets GetBody for the
// byte-slice bodies the provider clients send), so a streaming request
// retries connection and status failures but never re-opens mid-stream.
// Retry-After hints from 429/503 responses are honored.
func NewRetryTransport(base http.RoundTripper, config RetryConfig) http.RoundTripper {
	if base == nil {
		base = NewTransport()
	}
	if config.MaxAttempts < 1 {
		config.MaxAttempts = 1
	}
	return &retryTransport{base: base, config: config}
}

type retryTransport struct {
	base   http.RoundTripper
	config RetryConfig
}

func (t *retryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	// Without GetBody the body cannot be replayed: one attempt, exactly
	// like a bare transport.
	if request.Body != nil && request.GetBody == nil {
		return t.base.RoundTrip(request)
	}

	var (
		response *http.Response
		err      error
	)
	for attempt := 1; ; attempt++ {
		attemptRequest := request.Clone(request.Context())
		if request.Body != nil {
			body, bodyErr := request.GetBody()
			if bodyErr != nil {
				return nil, bodyErr
			}
			attemptRequest.Body = body
		}

		response, err = t.base.RoundTrip(attemptRequest)
		if attempt >= t.config.MaxAttempts || !retryable(response, err) {
			return response, err
		}
		if response != nil && response.Body != nil {
			// Drain so the pooled connection can be reused.
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4*1024))
			response.Body.Close()
		}

		delay := t.backoff(attempt, response)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-request.Context().Done():
			timer.Stop()
			return nil, request.Context().Err()
		}
	}
}

// retryable reports whether the failure is transient: a transport error
// (dial reset, EOF, timeout — context cancellation is the caller's own
// doing and never retries) or a throttling/server status.
func retryable(response *http.Response, err error) bool {
	if err != nil {
		if strings.Contains(err.Error(), "context canceled") ||
			strings.Contains(err.Error(), "context deadline exceeded") {
			return false
		}
		return true
	}
	if response == nil {
		return false
	}
	switch {
	case response.StatusCode == http.StatusRequestTimeout,
		response.StatusCode == http.StatusTooManyRequests:
		return true
	case response.StatusCode >= 500:
		return true
	}
	return false
}

// backoff computes the sleep before the next attempt: exponential growth
// from BaseDelay with full jitter, superseded by a Retry-After hint when
// the server sends one.
func (t *retryTransport) backoff(attempt int, response *http.Response) time.Duration {
	if response != nil {
		if hint := retryAfter(response.Header.Get("Retry-After")); hint > 0 {
			return hint
		}
	}
	delay := min(t.config.BaseDelay<<(attempt-1), t.config.MaxDelay)
	if delay <= 0 {
		return 0
	}
	// Full jitter: uniform in [0, delay], avoiding synchronized retries.
	return time.Duration(rand.Int63n(int64(delay) + 1))
}

// retryAfter parses the Retry-After header's seconds form.
func retryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
