// Package telemetrytest installs an in-memory OpenTelemetry tracer provider so a
// test can assert what a package actually emitted: span names, attributes,
// events, and parent/child shape.
//
// It exists because instrumentation without assertions rots: a refactor can drop
// a span, rename an attribute or end a streaming span twice and nothing fails.
// Every package that emits spans ships at least one test using this kit.
//
// The kit only replaces the tracer provider. If the code under test also records
// metrics, those instruments must be resolved after New (instruments created at
// package init bind to the provider that existed at init).
package telemetrytest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Recorder captures finished spans in memory.
type Recorder struct {
	exporter *tracetest.InMemoryExporter
}

// New installs an in-memory tracer provider for the duration of the test and
// restores the previous provider on cleanup. Call it before the code under test
// starts spans: a tracer resolved before New keeps delegating to the old
// provider.
func New(t *testing.T) *Recorder {
	t.Helper()
	previous := otel.GetTracerProvider()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	return &Recorder{exporter: exporter}
}

// Spans returns the finished spans in completion order.
func (r *Recorder) Spans() []sdktrace.ReadOnlySpan {
	return r.exporter.GetSpans().Snapshots()
}

// Reset drops the captured spans, keeping the provider installed.
func (r *Recorder) Reset() {
	r.exporter.Reset()
}

// Named returns the finished spans whose name is exactly name, in completion
// order.
func (r *Recorder) Named(name string) []sdktrace.ReadOnlySpan {
	var matched []sdktrace.ReadOnlySpan
	for _, span := range r.Spans() {
		if span.Name() == name {
			matched = append(matched, span)
		}
	}
	return matched
}

// RequireNamed asserts exactly one finished span is named name and returns it.
//
// It fails with the names that were recorded instead, because "expected
// tool.execute" alone does not say whether the span was missing or renamed.
func (r *Recorder) RequireNamed(t *testing.T, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	matched := r.Named(name)
	if len(matched) == 1 {
		return matched[0]
	}
	names := make([]string, 0, len(r.Spans()))
	for _, span := range r.Spans() {
		names = append(names, span.Name())
	}
	t.Fatalf("expected exactly 1 span named %q, got %d; recorded: %v", name, len(matched), names)
	return nil
}

// RequireCount asserts exactly want finished spans are named name.
func (r *Recorder) RequireCount(t *testing.T, name string, want int) {
	t.Helper()
	if got := len(r.Named(name)); got != want {
		t.Fatalf("expected %d span(s) named %q, got %d", want, name, got)
	}
}

// Attrs indexes a span's attributes by key. Later values win, matching the SDK.
func Attrs(span sdktrace.ReadOnlySpan) map[string]attribute.Value {
	out := make(map[string]attribute.Value, len(span.Attributes()))
	for _, kv := range span.Attributes() {
		out[string(kv.Key)] = kv.Value
	}
	return out
}

// RequireAttr asserts the span carries key with the given value, and fails with
// the full attribute set otherwise: a wrong value and a missing key are the same
// symptom (a producer that stopped emitting) until you see the rest.
func RequireAttr(t *testing.T, span sdktrace.ReadOnlySpan, key string, want attribute.Value) {
	t.Helper()
	attrs := Attrs(span)
	got, ok := attrs[key]
	if ok && got.Type() == want.Type() && got.String() == want.String() {
		return
	}
	t.Fatalf("span %q: attribute %q = %v (present=%t), want %v; all attrs: %v",
		span.Name(), key, got.String(), ok, want.String(), attrs)
}

// RequireAttrString is RequireAttr for the common string-typed attribute.
func RequireAttrString(t *testing.T, span sdktrace.ReadOnlySpan, key, want string) {
	t.Helper()
	RequireAttr(t, span, key, attribute.StringValue(want))
}

// Events returns the span events named name, in order.
func Events(span sdktrace.ReadOnlySpan, name string) []sdktrace.Event {
	var matched []sdktrace.Event
	for _, event := range span.Events() {
		if event.Name == name {
			matched = append(matched, event)
		}
	}
	return matched
}

// RequireEventCount asserts the span recorded exactly want events named name.
func RequireEventCount(t *testing.T, span sdktrace.ReadOnlySpan, name string, want int) {
	t.Helper()
	if got := len(Events(span, name)); got != want {
		t.Fatalf("span %q: expected %d event(s) named %q, got %d", span.Name(), want, name, got)
	}
}
