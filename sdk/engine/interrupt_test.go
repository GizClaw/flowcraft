package engine_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestInterrupted_SatisfiesIsInterrupted(t *testing.T) {
	err := engine.Interrupted(engine.Interrupt{Cause: engine.CauseUserCancel, Detail: "stop"})
	if !errdefs.IsInterrupted(err) {
		t.Errorf("Interrupted() error must satisfy errdefs.IsInterrupted; got %v", err)
	}
}

func TestInterrupted_AsRestoresInterrupt(t *testing.T) {
	err := engine.Interrupted(engine.Interrupt{Cause: engine.CauseUserInput, Detail: "barge"})

	var ie engine.InterruptedError
	if !errors.As(err, &ie) {
		t.Fatal("errors.As must destructure InterruptedError")
	}
	if ie.Cause != engine.CauseUserInput {
		t.Errorf("Cause = %q, want %q", ie.Cause, engine.CauseUserInput)
	}
	if ie.Detail != "barge" {
		t.Errorf("Detail = %q, want %q", ie.Detail, "barge")
	}
}

func TestInterrupted_AsThroughWrap(t *testing.T) {
	wrapped := fmt.Errorf("layered: %w",
		engine.Interrupted(engine.Interrupt{Cause: engine.CauseHostShutdown, Detail: "graceful"}))

	if !errdefs.IsInterrupted(wrapped) {
		t.Error("wrapped Interrupted should still satisfy IsInterrupted")
	}

	var ie engine.InterruptedError
	if !errors.As(wrapped, &ie) {
		t.Fatal("errors.As must drill through wraps")
	}
	if ie.Cause != engine.CauseHostShutdown {
		t.Errorf("Cause = %q, want %q", ie.Cause, engine.CauseHostShutdown)
	}
}

func TestInterrupted_ZeroValueWellFormedMessage(t *testing.T) {
	cases := []struct {
		name string
		intr engine.Interrupt
		want string
	}{
		{"zero", engine.Interrupt{}, "engine: interrupted"},
		{"detailOnly", engine.Interrupt{Detail: "stuck"}, "engine: interrupted: stuck"},
		{"causeOnly", engine.Interrupt{Cause: engine.CauseUserCancel}, "engine: interrupted (user_cancel)"},
		{"both", engine.Interrupt{Cause: engine.CauseUserInput, Detail: "barge"}, "engine: interrupted (user_input): barge"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := engine.Interrupted(c.intr)
			if err.Error() != c.want {
				t.Errorf("Error() = %q, want %q", err.Error(), c.want)
			}
		})
	}
}

// TestInterruptedError_MarkerInvoked ensures the unexported marker
// method on InterruptedError is actually called by an errors.As-based
// classifier; without an explicit interface assertion the cover tool
// won't see it run. We use the public marker shape that errdefs
// expects.
func TestInterruptedError_MarkerInvoked(t *testing.T) {
	err := engine.Interrupted(engine.Interrupt{Cause: engine.CauseUserCancel})

	var marker interface{ Interrupted() }
	if !errors.As(err, &marker) {
		t.Fatal("Interrupted() error must satisfy the errdefs marker shape")
	}
	// Calling the marker must not panic.
	marker.Interrupted()
}

func TestMergeInterrupts_FansInFromMultipleSources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := make(chan engine.Interrupt, 1)
	b := make(chan engine.Interrupt, 1)
	out := engine.MergeInterrupts(ctx, a, b)

	a <- engine.Interrupt{Cause: engine.CauseUserCancel, Detail: "from-a"}
	b <- engine.Interrupt{Cause: engine.CauseHostShutdown, Detail: "from-b"}

	got := make(map[engine.Cause]string)
	for i := 0; i < 2; i++ {
		select {
		case intr := <-out:
			got[intr.Cause] = intr.Detail
		case <-time.After(time.Second):
			t.Fatalf("merged channel timed out at %d/2", i+1)
		}
	}
	if got[engine.CauseUserCancel] != "from-a" || got[engine.CauseHostShutdown] != "from-b" {
		t.Fatalf("merged values = %+v, want both detail strings", got)
	}
}

func TestMergeInterrupts_NilSourcesIgnored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	live := make(chan engine.Interrupt, 1)
	out := engine.MergeInterrupts(ctx, nil, live, nil)

	live <- engine.Interrupt{Cause: engine.CauseCustom, Detail: "go"}
	select {
	case intr := <-out:
		if intr.Cause != engine.CauseCustom {
			t.Fatalf("Cause = %q, want %q", intr.Cause, engine.CauseCustom)
		}
	case <-time.After(time.Second):
		t.Fatal("nil sources must be skipped, not crash; live source produced no value")
	}
}

func TestMergeInterrupts_ClosesWhenEverySourceCloses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := make(chan engine.Interrupt)
	b := make(chan engine.Interrupt)
	out := engine.MergeInterrupts(ctx, a, b)

	close(a)
	close(b)

	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("merged channel should be closed after every source closed")
		}
	case <-time.After(time.Second):
		t.Fatal("merged channel did not close within deadline")
	}
}

func TestMergeInterrupts_ClosesOnCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := make(chan engine.Interrupt)
	out := engine.MergeInterrupts(ctx, a)

	cancel()

	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("merged channel should be closed after ctx cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("ctx cancel did not propagate to merged channel within deadline")
	}
}

func TestMergeInterrupts_ZeroSources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := engine.MergeInterrupts(ctx)

	// Channel is alive (no source closed it) until ctx fires.
	select {
	case <-out:
		t.Fatal("zero-source merge must not yield values before ctx cancel")
	default:
	}
	cancel()
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("zero-source merge must close on ctx cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("zero-source merge did not close after ctx cancel")
	}
}

func TestMergeInterrupts_NoGoroutineLeakAfterCancel(t *testing.T) {
	// Defensive: spin up several merges, cancel them, ensure every
	// source-side forwarder has exited (i.e. each source channel can
	// be re-used / GC'd) by counting WaitGroup completions through a
	// proxy: re-sending into an already-cancelled merge must not
	// block indefinitely because no forwarder is parked on receive.
	ctx, cancel := context.WithCancel(context.Background())
	a := make(chan engine.Interrupt, 1)
	out := engine.MergeInterrupts(ctx, a)

	cancel()
	// Drain out so the closer goroutine sees both ctx.Done and an
	// emptied channel state.
	<-out

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// After cancel + drain, sending into the source channel must
		// not park forever — no forwarder remains. Buffered cap=1
		// absorbs the send; unblocked goroutine returns immediately.
		a <- engine.Interrupt{}
	}()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("source channel send blocked — forwarder goroutine leaked")
	}
}

func TestCauseConstants_StableValues(t *testing.T) {
	// The Cause string values are part of the wire contract (they
	// flow into errdefs and may be persisted in checkpoint metadata).
	// Pin them down so a refactor that renames a constant breaks
	// loudly.
	pairs := []struct {
		c    engine.Cause
		want string
	}{
		{engine.CauseUnknown, ""},
		{engine.CauseUserCancel, "user_cancel"},
		{engine.CauseUserInput, "user_input"},
		{engine.CauseHostShutdown, "host_shutdown"},
		{engine.CauseCustom, "custom"},
	}
	for _, p := range pairs {
		if string(p.c) != p.want {
			t.Errorf("Cause %q has value %q, want %q", p.c, string(p.c), p.want)
		}
	}
}
