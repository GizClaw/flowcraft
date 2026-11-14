package httpkit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestRoundTripperInjectsTraceContext pins the cross-process half of tracing: a
// request that leaves without traceparent starts a second trace on the far side,
// and two well-formed traces that join nothing are harder to notice than none.
func TestRoundTripperInjectsTraceContext(t *testing.T) {
	installTestPropagation(t)

	var received string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	ctx, span := otel.Tracer("test").Start(context.Background(), "parent")
	defer span.End()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	response, err := NewHTTPClient().Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = response.Body.Close()

	want := "00-" + span.SpanContext().TraceID().String() + "-" +
		span.SpanContext().SpanID().String() + "-01"
	if received != want {
		t.Fatalf("traceparent = %q, want %q", received, want)
	}
	// The transport clones: the caller's request is not ours to mutate, and a
	// retry above may replay it.
	if got := request.Header.Get("traceparent"); got != "" {
		t.Fatalf("the caller's request was mutated: %q", got)
	}
}

// TestRetryTransportRecordsAttempts pins that a retried request is visible as
// retries, not as one slow call.
func TestRetryTransportRecordsAttempts(t *testing.T) {
	recorder := installTestPropagation(t)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := NewHTTPClient(WithRetry(RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Millisecond,
	}))

	ctx, span := otel.Tracer("test").Start(context.Background(), "retried")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = response.Body.Close()
	span.End()

	if got := requests.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
	var retries []sdktrace.Event
	for _, event := range recorder.Ended()[0].Events() {
		if event.Name == "http.retry" {
			retries = append(retries, event)
		}
	}
	if len(retries) != 2 {
		t.Fatalf("http.retry events = %d, want 2", len(retries))
	}
	attrs := map[string]any{}
	for _, kv := range retries[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.AsInterface()
	}
	if attrs["http.retry.attempt"] != int64(2) {
		t.Errorf("first retry attempt = %v, want 2", attrs["http.retry.attempt"])
	}
	if attrs["http.response.status_code"] != int64(http.StatusServiceUnavailable) {
		t.Errorf("first retry status = %v, want 503", attrs["http.response.status_code"])
	}
}

// installTestPropagation installs a sampling tracer provider and the W3C
// propagator, restoring both globals on cleanup.
func installTestPropagation(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})
	return recorder
}
