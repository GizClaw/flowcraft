package graph

import (
	"context"
	"testing"
	"time"
)

func TestForkController_CancelRunningPropagatesCause(t *testing.T) {
	c := newForkController([]string{"a", "b"})
	ctx, cancel := context.WithCancelCause(context.Background())
	if _, cancelled := c.start("b", cancel); cancelled {
		t.Fatal("b unexpectedly cancelled at start")
	}

	if !c.view("a").CancelNode("b", "enough") {
		t.Fatal("CancelNode(b) = false, want true")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("branch context not cancelled")
	}
	if cause := context.Cause(ctx); cause == nil || cause.Error() != "enough" {
		t.Fatalf("context cause = %v, want the cancel reason", cause)
	}
	if reason, ok := c.cancelledReason("b"); !ok || reason != "enough" {
		t.Fatalf("cancelledReason = %q/%v", reason, ok)
	}
}

func TestForkController_CancelQueuedSkipsStart(t *testing.T) {
	c := newForkController([]string{"a", "b"})
	if !c.view("a").CancelNode("b", "not needed") {
		t.Fatal("CancelNode on queued branch = false")
	}
	// b starts later (it was stuck in the semaphore): the mark must
	// stop the invocation.
	if reason, cancelled := c.start("b", context.CancelCauseFunc(func(error) {})); !cancelled || reason != "not needed" {
		t.Fatalf("start = %q/%v, want the queued cancellation", reason, cancelled)
	}
}

func TestForkController_UnknownAndFinishedReportFalse(t *testing.T) {
	c := newForkController([]string{"a", "b"})
	if c.view("a").CancelNode("ghost", "x") {
		t.Fatal("unknown node cancelled")
	}
	_, cancel := context.WithCancelCause(context.Background())
	c.start("b", cancel)
	c.finish("b")
	if c.view("a").CancelNode("b", "too late") {
		t.Fatal("finished branch cancelled")
	}
}

func TestForkController_CancelledCallerLosesRights(t *testing.T) {
	c := newForkController([]string{"a", "b", "c"})
	_, cancelA := context.WithCancelCause(context.Background())
	_, cancelC := context.WithCancelCause(context.Background())
	c.start("a", cancelA)
	c.start("c", cancelC)

	if !c.view("b").CancelNode("a", "done") {
		t.Fatal("CancelNode(a) = false")
	}
	// a is cancelled: it may no longer cancel others.
	if c.view("a").CancelNode("c", "again") {
		t.Fatal("a cancelled branch cancelled another node")
	}
	if _, marked := c.cancelledReason("c"); marked {
		t.Fatal("c was marked by a disqualified caller")
	}
	// A healthy branch still can.
	if !c.view("b").CancelNode("c", "also done") {
		t.Fatal("healthy branch could not cancel c")
	}
}

func TestForkController_IdempotentDefaultReason(t *testing.T) {
	c := newForkController([]string{"a", "b"})
	_, cancel := context.WithCancelCause(context.Background())
	c.start("b", cancel)

	if !c.view("a").CancelNode("b", "") {
		t.Fatal("first cancel = false")
	}
	if !c.view("a").CancelNode("b", "second") {
		t.Fatal("repeat cancel = false, want idempotent true")
	}
	// First reason wins.
	if reason, _ := c.cancelledReason("b"); reason != "cancelled by parallel.cancelNode" {
		t.Fatalf("reason = %q, want the default from the first call", reason)
	}
}
