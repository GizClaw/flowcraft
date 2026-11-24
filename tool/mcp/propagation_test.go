package mcp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type stubRoundTripper func(*http.Request) (*http.Response, error)

func (f stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestHeaderRoundTripperInjectsTraceContext pins the MCP half of propagation.
// The MCP SDK builds the request, so this round tripper is the only place the
// traceparent header can be added; without it the MCP hop is the one hop no
// trace crosses.
func TestHeaderRoundTripperInjectsTraceContext(t *testing.T) {
	installTestPropagation(t)

	var traceparent, static string
	transport := &headerRoundTripper{
		base: stubRoundTripper(func(req *http.Request) (*http.Response, error) {
			traceparent = req.Header.Get("traceparent")
			static = req.Header.Get("X-Static")
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		}),
		headers: map[string]string{"X-Static": "1"},
	}

	ctx, span := otel.Tracer("test").Start(context.Background(), "mcp")
	defer span.End()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://example.test", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if _, err := transport.RoundTrip(request); err != nil {
		t.Fatalf("round trip: %v", err)
	}

	if want := span.SpanContext().TraceID().String(); !strings.Contains(traceparent, want) {
		t.Fatalf("traceparent = %q, want it to carry %q", traceparent, want)
	}
	if static != "1" {
		t.Fatalf("static header = %q, want %q", static, "1")
	}
	// The round tripper clones: the caller's request must not gain headers.
	if got := request.Header.Get("traceparent"); got != "" {
		t.Fatalf("the caller's request was mutated: %q", got)
	}
}

// installTestPropagation installs a sampling tracer provider and the W3C
// propagator, restoring both globals on cleanup.
func installTestPropagation(t *testing.T) {
	t.Helper()
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})
}
