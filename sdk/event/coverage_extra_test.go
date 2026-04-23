package event

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestMemoryBus_Publish_AutoFillsIDAndTime confirms Publish populates
// Envelope.ID and Envelope.Time for callers who hand it a partially
// constructed envelope. NewEnvelope already fills these for normal
// call sites; Publish has its own fill so that bus implementations
// remain interchangeable when an upstream layer hands envelopes
// through directly (e.g. a remote bridge replaying from the wire).
func TestMemoryBus_Publish_AutoFillsIDAndTime(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	sub, err := bus.Subscribe(ctx, Pattern("demo.>"))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()

	// Bypass NewEnvelope so the auto-fill in Publish is the code
	// under test.
	env := Envelope{Subject: "demo.fill"}
	if err := bus.Publish(ctx, env); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-sub.C():
		if got.ID == "" {
			t.Error("Publish should have populated ID")
		}
		if got.Time.IsZero() {
			t.Error("Publish should have populated Time")
		}
	case <-time.After(time.Second):
		t.Fatal("delivery timed out")
	}
}

// TestMemoryBus_Publish_AfterCloseFastFails covers the documented
// fast-fail path: Publish on a Bus that has already returned from
// Close must yield ErrBusClosed without registering inflight work
// or scanning subscriptions.
func TestMemoryBus_Publish_AfterCloseFastFails(t *testing.T) {
	bus := NewMemoryBus()
	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := bus.Publish(context.Background(), Envelope{Subject: "demo.x"})
	if !errors.Is(err, ErrBusClosed) {
		t.Fatalf("Publish after Close = %v, want ErrBusClosed", err)
	}
}

// TestMemoryBus_Publish_DropOldestRetrySucceeds drives the buffer
// path where the first non-blocking send fails (buffer full), one
// oldest envelope is evicted, and the retry succeeds — so the
// subscription still receives the newest envelope. Without this
// test the inner select that retries after the eviction has no
// dedicated coverage; the existing DropOldest test only confirms
// that *some* envelope eventually lands.
func TestMemoryBus_Publish_DropOldestRetrySucceeds(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	sub, err := bus.Subscribe(ctx, Pattern("demo.>"),
		WithBufferSize(1), WithBackpressure(DropOldest))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()

	for i := 0; i < 5; i++ {
		env, err := NewEnvelope(ctx, Subject("demo.x"), map[string]int{"i": i})
		if err != nil {
			t.Fatalf("NewEnvelope: %v", err)
		}
		if err := bus.Publish(ctx, env); err != nil {
			t.Fatalf("Publish #%d: %v", i, err)
		}
	}

	// The buffer is size-1 with DropOldest, so the very last publish
	// must have evicted older entries — the surviving envelope
	// carries i=4.
	select {
	case got := <-sub.C():
		var p map[string]int
		if err := got.Decode(&p); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if p["i"] != 4 {
			t.Fatalf("got i=%d, want 4 (DropOldest should keep the newest)", p["i"])
		}
	case <-time.After(time.Second):
		t.Fatal("expected at least one envelope, got none")
	}
}

// TestSubscription_IDIsUniqueAndStable asserts the contract that
// MemoryBus assigns each subscription a non-empty ID and that the
// IDs differ across subscriptions. Other tests rely on it implicitly
// (Observer routes drops by sub ID); this test pins it explicitly so
// a regression surfaces here rather than as a flaky downstream test.
func TestSubscription_IDIsUniqueAndStable(t *testing.T) {
	bus := NewMemoryBus()
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	const n = 8
	ids := make(map[SubscriptionID]struct{}, n)
	subs := make([]Subscription, n)
	for i := 0; i < n; i++ {
		s, err := bus.Subscribe(ctx, Pattern(">"))
		if err != nil {
			t.Fatalf("Subscribe[%d]: %v", i, err)
		}
		id := s.ID()
		if id == "" {
			t.Fatalf("Subscribe[%d] returned empty ID", i)
		}
		if _, dup := ids[id]; dup {
			t.Fatalf("duplicate SubscriptionID %q at index %d", id, i)
		}
		ids[id] = struct{}{}
		subs[i] = s

		// ID must remain stable across calls.
		if s.ID() != id {
			t.Fatalf("ID() not stable: %q vs %q", s.ID(), id)
		}
	}
	for _, s := range subs {
		_ = s.Close()
	}
}

// TestPattern_Matches_MalformedTrailIsLiteral guards the documented
// fallback in Pattern.Matches: if a malformed pattern slips through
// (a '>' segment that is not the last one), the matcher MUST treat
// that '>' as a literal segment so behaviour stays defined. The
// happy path is verified by Pattern.Validate; this test covers the
// safety net.
func TestPattern_Matches_MalformedTrailIsLiteral(t *testing.T) {
	p := Pattern("a.>.c")

	if !p.Matches(Subject("a.>.c")) {
		t.Errorf("malformed pattern should still literally match %q", "a.>.c")
	}
	if p.Matches(Subject("a.b.c")) {
		t.Errorf("malformed pattern must not behave as a wildcard for %q", "a.b.c")
	}
	if p.Matches(Subject("a")) {
		t.Errorf("length mismatch should still fail to match")
	}
}
