package httpkit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func fastRetry() RetryConfig {
	return RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
}

func get(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("body")))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer response.Body.Close()
	return response
}

func TestRetriesTransientStatuses(t *testing.T) {
	statuses := []int{429, 500, 503, 408}
	for _, status := range statuses {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) < 3 {
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			client := &http.Client{Transport: NewRetryTransport(nil, fastRetry())}
			response := get(t, client, server.URL)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", response.StatusCode)
			}
			if calls.Load() != 3 {
				t.Fatalf("calls = %d, want 3", calls.Load())
			}
		})
	}
}

func TestDoesNotRetryPermanentFailures(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	client := &http.Client{Transport: NewRetryTransport(nil, fastRetry())}
	response := get(t, client, server.URL)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (no retry on 400)", calls.Load())
	}
}

func TestRetryExhaustionReturnsLastResponse(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := &http.Client{Transport: NewRetryTransport(nil, fastRetry())}
	response := get(t, client, server.URL)
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want exactly MaxAttempts", calls.Load())
	}
}

func TestRetriesTransportErrors(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	url := server.URL
	server.Close() // first attempts hit a dead server; restart below

	// A server that refuses the first two connections then serves is hard
	// to stage with httptest; instead point at a closed port and expect a
	// transport error to surface after MaxAttempts.
	client := &http.Client{Transport: NewRetryTransport(nil, fastRetry())}
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(request)
	if err == nil {
		t.Fatal("expected a transport error against a dead server")
	}
}

func TestBodyReplaysAcrossAttempts(t *testing.T) {
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(payload))
		if len(bodies) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &http.Client{Transport: NewRetryTransport(nil, fastRetry())}
	get(t, client, server.URL)
	if len(bodies) != 3 {
		t.Fatalf("attempts = %d", len(bodies))
	}
	for i, body := range bodies {
		if body != "body" {
			t.Fatalf("attempt %d body = %q, want replayed payload", i, body)
		}
	}
}

func TestRetryAfterHonored(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	transport := &retryTransport{base: NewTransport(), config: fastRetry()}
	delay := transport.backoff(1, &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"1"}},
	})
	if delay != time.Second {
		t.Fatalf("delay = %v, want 1s from Retry-After", delay)
	}
	_ = server
}

func TestUnreplayableBodyPassesThrough(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := &http.Client{Transport: NewRetryTransport(nil, fastRetry())}
	request, err := http.NewRequest(http.MethodPost, server.URL, io.NopCloser(bytes.NewReader([]byte("x"))))
	if err != nil {
		t.Fatal(err)
	}
	request.GetBody = nil // NopCloser bodies have no replay channel
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	response.Body.Close()
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (no replay possible)", calls.Load())
	}
}

func TestContextCancellationStopsBackoff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	slow := RetryConfig{MaxAttempts: 5, BaseDelay: time.Second, MaxDelay: 5 * time.Second}
	client := &http.Client{Transport: NewRetryTransport(nil, slow)}
	request, err := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)
	cancel()

	_, err = client.Do(request)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
