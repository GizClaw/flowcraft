package runctx_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/correlation"
	"github.com/GizClaw/flowcraft/core/runtime/runctx"
)

// The deprecated package is a forwarding shim for one release: an outside
// consumer that still imports it must keep compiling *and* keep observing the
// same context value the new package writes. Testing both directions is the
// contract.
func TestShimForwardsToCorrelation(t *testing.T) {
	var run = correlation.Run{RuntimeID: "rt-1", TenantID: "acme"}
	ctx := runctx.WithRun(context.Background(), run)

	got, ok := correlation.From(ctx)
	if !ok || got.RuntimeID != "rt-1" || got.TenantID != "acme" {
		t.Fatalf("correlation.From(shim ctx) = %+v, %v", got, ok)
	}
	if shimmed, ok := runctx.From(ctx); !ok || shimmed != got {
		t.Fatalf("runctx.From = %+v, %v", shimmed, ok)
	}
	if attrs := runctx.Attrs(ctx); len(attrs) != 2 {
		t.Fatalf("runctx.Attrs = %v, want the two identity attributes", attrs)
	}
	if attrs := runctx.Attrs(context.Background()); attrs != nil {
		t.Fatalf("runctx.Attrs on an empty context = %v, want nil", attrs)
	}
}
