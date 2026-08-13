package event

import "testing"

func TestBackpressurePolicy_String(t *testing.T) {
	cases := []struct {
		p    BackpressurePolicy
		want string
	}{
		{DropNewest, "drop_newest"},
		{DropOldest, "drop_oldest"},
		{Block, "block"},
		{Sample, "sample"},
		{BackpressurePolicy(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.p.String(); got != c.want {
			t.Errorf("BackpressurePolicy(%d).String() = %q, want %q", c.p, got, c.want)
		}
	}
}

func TestDropReason_String(t *testing.T) {
	cases := []struct {
		r    DropReason
		want string
	}{
		{DropReasonBufferFull, "buffer_full"},
		{DropReasonClosed, "closed"},
		{DropReasonSampled, "sampled"},
		{DropReason(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.r.String(); got != c.want {
			t.Errorf("DropReason(%d).String() = %q, want %q", c.r, got, c.want)
		}
	}
}

// TestNoopObserver_Methods pins the contract that noopObserver is a
// total no-op: each method accepts the documented argument shape and
// returns without panicking. The struct is unexported but is the
// fallback when MemoryBus has no Observer configured, so a regression
// here would silently surface as a hot-path nil dereference.
func TestNoopObserver_Methods(t *testing.T) {
	var o noopObserver
	env := Envelope{Subject: "x"}
	o.OnPublish(env)
	o.OnDeliver("sub-id", env)
	o.OnDrop("sub-id", env, DropReasonBufferFull)
	o.OnDrop("sub-id", env, DropReasonClosed)
	o.OnDrop("sub-id", env, DropReasonSampled)
}
