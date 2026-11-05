package journal

import "github.com/GizClaw/flowcraft/core/sandbox"

// maxLosses bounds the loss ledger. Losses are rare and each entry is
// only a boundary; when the ledger is full the two oldest boundaries
// collapse into the earlier one, which over-reports (the reported
// first-missing seq moves earlier, never later) and therefore stays on
// the safe side of the bargain.
const maxLosses = 8

// lossEntry records that coverage is broken after seq After: events
// happened that this journal never recorded, and how many is unknown.
// Retention drops do not appear here — the ring's own oldest retained
// seq already says everything there is to say about them.
type lossEntry struct {
	after  int64
	reason sandbox.JournalGapReason
}

// ring is the bounded, replayable event window plus the loss ledger.
// It is guarded by the journal mutex; every method below assumes it.
type ring struct {
	retention int
	buf       []sandbox.WriteEvent
	head      int
	size      int
	next      int64
	losses    []lossEntry
}

func newRing(retention int) *ring {
	if retention <= 0 {
		retention = sandbox.DefaultJournalRetention
	}
	return &ring{
		retention: retention,
		buf:       make([]sandbox.WriteEvent, retention),
		next:      1,
	}
}

// append assigns the next seq to ev and stores it, evicting the oldest
// events once the retention window is full. Eviction is silent here:
// readers discover the hole from first(), which is why a full window
// never blocks the watcher.
func (r *ring) append(ev sandbox.WriteEvent) {
	ev.Seq = r.next
	r.next++
	r.buf[r.head] = ev
	r.head = (r.head + 1) % r.retention
	if r.size < r.retention {
		r.size++
	}
}

// high is the newest assigned seq, 0 before anything was recorded.
func (r *ring) high() int64 { return r.next - 1 }

// first is the oldest retained seq, or high+1 when the window is empty.
func (r *ring) first() int64 {
	if r.size == 0 {
		return r.next
	}
	start := r.head - r.size
	if start < 0 {
		start += r.retention
	}
	return r.buf[start].Seq
}

// after returns up to max retained events with seq >= from, ascending,
// in a fresh slice the caller owns.
func (r *ring) after(from int64, max int) []sandbox.WriteEvent {
	if r.size == 0 || max <= 0 {
		return nil
	}
	first := r.first()
	if from < first {
		from = first
	}
	if from > r.high() {
		return nil
	}
	skip := int(from - first)
	count := r.size - skip
	if count > max {
		count = max
	}
	start := r.head - r.size + skip
	if start < 0 {
		start += r.retention
	}
	out := make([]sandbox.WriteEvent, count)
	for i := range out {
		out[i] = r.buf[(start+i)%r.retention]
	}
	return out
}

// recordLoss appends a boundary: from here on, coverage is incomplete.
// The boundary is attached to the current high watermark, so a reader
// learns "everything up to here is accounted for; after here is not" —
// an overflow cannot say how much was dropped, and pretending to a seq
// range would invent precision the kernel never gave us.
func (r *ring) recordLoss(reason sandbox.JournalGapReason) {
	after := r.high()
	if n := len(r.losses); n > 0 && r.losses[n-1].after == after {
		// Same boundary, same statement: keep the first reason, which is
		// the one that explains the original break.
		return
	}
	if len(r.losses) >= maxLosses {
		// Drop the second-oldest boundary, keeping the oldest: the
		// merged entry reports an earlier first-missing seq than the
		// truth, which over-reports rather than hides.
		r.losses = append(r.losses[:1], r.losses[2:]...)
	}
	r.losses = append(r.losses, lossEntry{after: after, reason: reason})
}

// readView answers one Read call: the events the reader is owed, its
// next cursor, and the gap accounting for the difference.
//
// The invariant is that (afterSeq, NextSeq] is fully accounted for —
// every seq in it was either returned or declared lost — so a consumer
// can advance its cursor without ever skipping a seq silently.
func (r *ring) readView(afterSeq int64, max int, frozen bool) sandbox.JournalBatch {
	batch := sandbox.JournalBatch{NextSeq: afterSeq, Closed: frozen}
	start := afterSeq + 1
	first := r.first()
	high := r.high()

	// The reader is behind the retention window: everything from its
	// cursor up to the oldest retained event is gone, and that hole is
	// the earliest loss this window can report.
	behind := start < first && start <= high
	if behind {
		batch.Gap = &sandbox.JournalGap{FirstMissing: start, Reason: sandbox.JournalGapRetention}
		start = first
	}

	events := r.after(start, max)
	if len(events) > 0 {
		batch.Events = events
		batch.NextSeq = events[len(events)-1].Seq
	} else if behind {
		// Nothing left to serve and the whole hole was declared: the
		// cursor may jump past it, so the next read starts fresh
		// instead of re-reporting the same dead range forever.
		batch.NextSeq = high
	}

	for _, l := range r.losses {
		if l.after < afterSeq {
			continue
		}
		firstMissing := l.after + 1
		if batch.Gap == nil || firstMissing < batch.Gap.FirstMissing {
			batch.Gap = &sandbox.JournalGap{FirstMissing: firstMissing, Reason: l.reason}
		}
	}
	return batch
}

// release drops the retained events. Called once the journal is frozen
// and the last reader is gone, so a closed runner stops holding its
// window in memory.
func (r *ring) release() {
	r.buf = nil
	r.head = 0
	r.size = 0
}
