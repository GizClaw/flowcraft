package runctx

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"

	"github.com/GizClaw/flowcraft/core/telemetry"
)

func TestRunRoundTrip(t *testing.T) {
	ctx := WithRun(context.Background(), Run{RuntimeID: "rt-1", TenantID: "acme"})
	run, ok := From(ctx)
	if !ok {
		t.Fatal("RunFrom reported nothing")
	}
	if run.RuntimeID != "rt-1" || run.TenantID != "acme" {
		t.Errorf("run = %+v", run)
	}
}

// The two facts are discovered at different depths: a layer that knows the
// tenant must not erase the runtime id it inherited.
func TestRunMergesNestedCalls(t *testing.T) {
	ctx := WithRun(context.Background(), Run{RuntimeID: "rt-1"})
	ctx = WithRun(ctx, Run{TenantID: "acme"})
	run, _ := From(ctx)
	if run.RuntimeID != "rt-1" || run.TenantID != "acme" {
		t.Errorf("run = %+v, want both fields", run)
	}
}

func TestAttrsOmitsEmptyFields(t *testing.T) {
	ctx := WithRun(context.Background(), Run{RuntimeID: "rt-1"})
	attrs := Attrs(ctx)
	if len(attrs) != 1 {
		t.Fatalf("attrs = %v, want only the runtime id", attrs)
	}
	if string(attrs[0].Key) != telemetry.AttrRuntimeID || attrs[0].Value.AsString() != "rt-1" {
		t.Errorf("attr = %v, want %s=rt-1", attrs[0], telemetry.AttrRuntimeID)
	}

	both := Attrs(WithRun(context.Background(), Run{RuntimeID: "rt-1", TenantID: "acme"}))
	if len(both) != 2 {
		t.Fatalf("attrs = %v, want two", both)
	}
	if string(both[1].Key) != telemetry.AttrTenantID {
		t.Errorf("second attr = %s, want %s", both[1].Key, telemetry.AttrTenantID)
	}
}

// A hand-built context stays free: no attachment means no allocation and no
// attributes to filter.
func TestAttrsAbsent(t *testing.T) {
	if attrs := Attrs(context.Background()); attrs != nil {
		t.Errorf("attrs = %v, want nil", attrs)
	}
	var absent context.Context
	if attrs := Attrs(absent); attrs != nil {
		t.Errorf("attrs on nil ctx = %v, want nil", attrs)
	}
	if _, ok := From(absent); ok {
		t.Error("From(nil) reported a run")
	}
}

func TestWithRunOnNilContext(t *testing.T) {
	//nolint:staticcheck // WithRun documents that it tolerates a nil context
	ctx := WithRun(nil, Run{RuntimeID: "rt-1"})
	if run, ok := From(ctx); !ok || run.RuntimeID != "rt-1" {
		t.Errorf("run = %+v, ok = %v", run, ok)
	}
}

// A runtime attaches its own id; a higher layer adds the tenant on the same
// context and neither field is lost.
func TestRunAddsTenantToRuntimeContext(t *testing.T) {
	ctx := WithRun(context.Background(), Run{RuntimeID: "rt-1"})
	ctx = WithRun(ctx, Run{TenantID: "acme"})
	attrs := Attrs(ctx)
	if len(attrs) != 2 {
		t.Fatalf("attrs = %v, want runtime id and tenant", attrs)
	}
}

// The attributes are built when the identity is attached: a span decorating the
// same context must not rebuild them.
func TestAttrsAllocateNothingOnceAttached(t *testing.T) {
	ctx := WithRun(context.Background(), Run{RuntimeID: "rt-1", TenantID: "tenant-1"})
	if got := testing.AllocsPerRun(1000, func() { _ = Attrs(ctx) }); got != 0 {
		t.Fatalf("Attrs allocates %.0f times, want 0", got)
	}
}

// The returned slice is shared, so a caller's append must copy rather than write
// into the cache.
func TestAttrsSurviveCallerAppend(t *testing.T) {
	ctx := WithRun(context.Background(), Run{RuntimeID: "rt-1"})
	attrs := Attrs(ctx)
	if got := len(append(attrs, attribute.String("extra", "x"))); got != 2 {
		t.Fatalf("append = %d attributes, want 2", got)
	}
	if got := len(Attrs(ctx)); got != 1 {
		t.Fatalf("Attrs after a caller append = %d attributes, want 1", got)
	}
}

func BenchmarkAttrs(b *testing.B) {
	ctx := WithRun(context.Background(), Run{RuntimeID: "rt-1", TenantID: "tenant-1"})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Attrs(ctx)
	}
}
