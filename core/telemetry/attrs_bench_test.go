package telemetry_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/telemetry"
)

// Identity rendering has no benchmark yet: telemetry.Attrs belongs to the identity
// batch, and this package cannot measure what does not exist. The pipeline and route
// benchmarks in the inference module already show what a span costs without it (~38
// and ~28 allocations per call), which is the number that will move when Attrs lands.

var attrSink telemetry.Attr

// TestAttrConstructorsDoNotAllocate keeps the value builders free: they are called
// for every attribute on every span, and a wrapper that allocated would multiply
// the cost of a span by its attribute count.
func TestAttrConstructorsDoNotAllocate(t *testing.T) {
	allocs := testing.AllocsPerRun(100, func() {
		attrSink = telemetry.String(telemetry.AttrErrorMessage, "boom")
		attrSink = telemetry.Int(telemetry.AttrSandboxExitCode, 1)
	})
	if allocs != 0 {
		t.Fatalf("attribute constructors allocate %.0f times per call", allocs)
	}
}
