package journal

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

func fill(r *ring, n int) {
	for i := 0; i < n; i++ {
		r.append(sandbox.WriteEvent{Op: sandbox.FileOpWrite})
	}
}

func seqs(events []sandbox.WriteEvent) []int64 {
	out := make([]int64, len(events))
	for i, ev := range events {
		out[i] = ev.Seq
	}
	return out
}

func TestRingAssignsContiguousSeqs(t *testing.T) {
	r := newRing(0)
	if got := r.high(); got != 0 {
		t.Fatalf("empty ring high = %d, want 0", got)
	}
	fill(r, 3)
	if got := r.high(); got != 3 {
		t.Fatalf("high = %d, want 3", got)
	}
	if got := r.first(); got != 1 {
		t.Fatalf("first = %d, want 1", got)
	}
	events := r.after(0, 10)
	if len(events) != 3 || events[0].Seq != 1 || events[2].Seq != 3 {
		t.Fatalf("after(0, 10) = %v, want seqs 1..3", seqs(events))
	}
}

func TestRingReadViewServesInBatches(t *testing.T) {
	r := newRing(0)
	fill(r, 3)

	first := r.readView(0, 2, false)
	if len(first.Events) != 2 || first.NextSeq != 2 || first.Gap != nil || first.Closed {
		t.Fatalf("first batch = %+v", first)
	}
	second := r.readView(first.NextSeq, 2, false)
	if len(second.Events) != 1 || second.NextSeq != 3 || second.Gap != nil {
		t.Fatalf("second batch = %+v", second)
	}
	idle := r.readView(second.NextSeq, 2, false)
	if len(idle.Events) != 0 || idle.NextSeq != 3 || idle.Gap != nil {
		t.Fatalf("idle batch = %+v", idle)
	}
}

func TestRingReadViewRetentionGap(t *testing.T) {
	r := newRing(2)
	fill(r, 5)

	batch := r.readView(0, 10, false)
	if batch.Gap == nil || batch.Gap.Reason != sandbox.JournalGapRetention {
		t.Fatalf("gap = %+v, want a retention gap", batch.Gap)
	}
	if batch.Gap.FirstMissing != 1 {
		t.Fatalf("FirstMissing = %d, want 1 (the exact first lost seq)", batch.Gap.FirstMissing)
	}
	if got := seqs(batch.Events); len(got) != 2 || got[0] != 4 || got[1] != 5 {
		t.Fatalf("events = %v, want the retained 4,5", got)
	}
	if batch.NextSeq != 5 {
		t.Fatalf("NextSeq = %d, want 5", batch.NextSeq)
	}

	// A reader whose cursor is inside the hole (it consumed seq 1, so
	// the hole starts for it at seq 2) learns exactly that, and gets
	// everything retained.
	inside := r.readView(1, 10, false)
	if inside.Gap == nil || inside.Gap.FirstMissing != 2 {
		t.Fatalf("inside gap = %+v, want FirstMissing 2", inside.Gap)
	}
	if got := seqs(inside.Events); len(got) != 2 || got[0] != 4 {
		t.Fatalf("inside events = %v", got)
	}

	// A reader that consumed up to the oldest retained seq has missed
	// nothing: eviction only takes what it already read.
	if consumed := r.readView(3, 10, false); consumed.Gap != nil {
		t.Fatalf("consumed gap = %+v, want none", consumed.Gap)
	}

	// A reader past the retained window sees no gap at all.
	fresh := r.readView(4, 10, false)
	if fresh.Gap != nil {
		t.Fatalf("fresh gap = %+v, want none", fresh.Gap)
	}
}

func TestRingReadViewEmptyWindowAccountsForTheHole(t *testing.T) {
	r := newRing(2)
	fill(r, 5)
	r.release()

	batch := r.readView(0, 10, false)
	if batch.Gap == nil || batch.Gap.Reason != sandbox.JournalGapRetention {
		t.Fatalf("gap = %+v, want a retention gap", batch.Gap)
	}
	if len(batch.Events) != 0 {
		t.Fatalf("events = %v, want none", seqs(batch.Events))
	}
	if batch.NextSeq != 5 {
		t.Fatalf("NextSeq = %d, want the hole accounted up to 5", batch.NextSeq)
	}
}

func TestRingReadViewAheadOfTheStream(t *testing.T) {
	r := newRing(0)
	fill(r, 2)

	batch := r.readView(7, 10, false)
	if len(batch.Events) != 0 || batch.NextSeq != 7 || batch.Gap != nil {
		t.Fatalf("batch = %+v, want an empty batch at the caller's cursor", batch)
	}
}

func TestRingReadViewFrozen(t *testing.T) {
	r := newRing(0)
	fill(r, 1)
	batch := r.readView(0, 10, true)
	if !batch.Closed {
		t.Fatal("Closed = false, want true for a frozen journal")
	}
}

func TestRingLossBoundaryReportsAnUnboundedGap(t *testing.T) {
	r := newRing(0)
	fill(r, 3)
	r.recordLoss(sandbox.JournalGapOverflow)

	// Drained to the boundary: the loss is reported as a state of the
	// stream, with FirstMissing past the returned cursor because how
	// much was lost is unknown.
	batch := r.readView(3, 10, false)
	if batch.Gap == nil || batch.Gap.Reason != sandbox.JournalGapOverflow {
		t.Fatalf("gap = %+v, want the overflow", batch.Gap)
	}
	if batch.Gap.FirstMissing != 4 {
		t.Fatalf("FirstMissing = %d, want 4 (the boundary)", batch.Gap.FirstMissing)
	}
	if batch.NextSeq != 3 {
		t.Fatalf("NextSeq = %d, want 3", batch.NextSeq)
	}

	// A reader that predates the boundary sees it too, alongside the
	// events that were recorded before it.
	behind := r.readView(0, 10, false)
	if behind.Gap == nil || behind.Gap.FirstMissing != 4 {
		t.Fatalf("behind gap = %+v", behind.Gap)
	}
	if len(behind.Events) != 3 {
		t.Fatalf("behind events = %v, want the 3 recorded before the loss", seqs(behind.Events))
	}
}

func TestRingLossBoundaryPickedAsTheEarliestLoss(t *testing.T) {
	r := newRing(2)
	fill(r, 4)
	r.recordLoss(sandbox.JournalGapCapacity)

	// Both a retention hole (seq 1,2 evicted) and a loss boundary exist:
	// the batch reports the earliest, which is the eviction.
	batch := r.readView(0, 10, false)
	if batch.Gap == nil || batch.Gap.Reason != sandbox.JournalGapRetention || batch.Gap.FirstMissing != 1 {
		t.Fatalf("gap = %+v, want the retention hole at seq 1", batch.Gap)
	}
}

func TestRingLossLedgerStaysBounded(t *testing.T) {
	r := newRing(0)
	for i := 0; i < 12; i++ {
		fill(r, 1)
		r.recordLoss(sandbox.JournalGapBackend)
	}
	if len(r.losses) > maxLosses {
		t.Fatalf("ledger grew to %d entries, want <= %d", len(r.losses), maxLosses)
	}
	// Collapsing keeps the oldest boundary, so the reported
	// first-missing seq is never later than the truth.
	if r.losses[0].after != 1 {
		t.Fatalf("oldest boundary = %d, want 1", r.losses[0].after)
	}
}

func TestRingRecordLossDeduplicatesSameBoundary(t *testing.T) {
	r := newRing(0)
	fill(r, 2)
	r.recordLoss(sandbox.JournalGapOverflow)
	r.recordLoss(sandbox.JournalGapCapacity)
	if len(r.losses) != 1 {
		t.Fatalf("ledger = %+v, want one entry for one boundary", r.losses)
	}
	if r.losses[0].reason != sandbox.JournalGapOverflow {
		t.Fatalf("reason = %v, want the first report", r.losses[0].reason)
	}
}

func TestRingAfterReturnsACopy(t *testing.T) {
	r := newRing(0)
	fill(r, 2)
	events := r.after(0, 10)
	events[0].Op = sandbox.FileOpRemove
	if r.buf[0].Op == sandbox.FileOpRemove {
		t.Fatal("after returned a slice aliasing the ring buffer")
	}
}
