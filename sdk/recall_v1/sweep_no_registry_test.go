package recall_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall_v1"
	"github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

// TestSweepOnce_NoSweeperOptionDoesNotPanic pins issue #160.
// Pre-fix, calling Memory.(*lt).SweepOnce on a Memory built
// WITHOUT WithSweeper (i.e. "I run TTL passes from my own
// scheduler") panicked with a nil-pointer dereference because
// nsRegistry was only initialised on the WithSweeper path.
//
// The fix in [New] now defaults nsRegistry to an in-memory
// implementation unconditionally; SweepOnce additionally carries
// a defensive nil-check that surfaces a clear error if a future
// refactor regresses the default.
func TestSweepOnce_NoSweeperOptionDoesNotPanic(t *testing.T) {
	idx := memory.New()
	mem, err := recall.New(idx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })

	type sweeper interface {
		SweepOnce(ctx context.Context) error
	}
	sw, ok := mem.(sweeper)
	if !ok {
		t.Fatalf("Memory implementation does not expose SweepOnce")
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("#160 regression: SweepOnce panicked without WithSweeper: %v", r)
		}
	}()
	// Should be a clean no-op (no expired docs, no registered
	// namespaces) — but most importantly, must not panic.
	if err := sw.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
}
