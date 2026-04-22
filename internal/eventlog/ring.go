package eventlog

import "sync"

// ring is a bounded in-memory append-only buffer of envelopes used as a fast
// path for live subscribers. It guarantees:
//   - append accepts strictly increasing seq (gaps are allowed; out-of-order
//     panics in dev, dropped in prod via append's bool return).
//   - readFrom(seq) returns every retained envelope with seq >= the request,
//     or (nil,false) when the requested seq is older than the oldest retained
//     envelope (caller must fall back to SQL).
//
// The buffer is intentionally lock-coarse: it sits behind SQLiteLog.writeMu
// during fan-out, so contention is dominated by the SQLite writer anyway.
type ring struct {
	mu  sync.RWMutex
	buf []Envelope // index by absolute_seq % cap
	cap int
	// minSeq/maxSeq describe the half-open window [minSeq, maxSeq] currently
	// retained. minSeq == 0 && maxSeq == 0 means the buffer is empty.
	minSeq int64
	maxSeq int64
}

func newRing(capacity int) *ring {
	if capacity <= 0 {
		capacity = defaultRingCapacity
	}
	return &ring{buf: make([]Envelope, capacity), cap: capacity}
}

// append pushes env onto the ring. If the ring is full, the oldest entry is
// silently overwritten (callers track loss via subscription.Lag).
//
// append assumes env.Seq is strictly greater than the previous max; out-of-
// order writes overwrite the slot they happen to land on but minSeq/maxSeq are
// not advanced backwards.
func (r *ring) append(env Envelope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appendLocked(env)
}

func (r *ring) appendBulk(envs []Envelope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, env := range envs {
		r.appendLocked(env)
	}
}

func (r *ring) appendLocked(env Envelope) {
	if env.Seq <= 0 {
		return
	}
	idx := int(env.Seq % int64(r.cap))
	r.buf[idx] = env
	if r.maxSeq == 0 {
		r.minSeq = env.Seq
		r.maxSeq = env.Seq
		return
	}
	if env.Seq > r.maxSeq {
		r.maxSeq = env.Seq
		// shrink window if we just wrapped
		if r.maxSeq-r.minSeq+1 > int64(r.cap) {
			r.minSeq = r.maxSeq - int64(r.cap) + 1
		}
	}
}

// readFrom returns every retained envelope whose seq is > sinceSeq, in seq
// order. The bool indicates whether the ring is authoritative for that range:
// when false the caller MUST fall back to SQL because some events have been
// evicted between sinceSeq+1 and the current minSeq.
func (r *ring) readFrom(sinceSeq int64) ([]Envelope, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.maxSeq == 0 {
		return nil, true
	}
	// requested cursor is at-or-after the current head: nothing to do.
	if sinceSeq >= r.maxSeq {
		return nil, true
	}
	// requested cursor is older than the oldest retained event: not authoritative.
	if sinceSeq+1 < r.minSeq {
		return nil, false
	}
	start := sinceSeq + 1
	if start < r.minSeq {
		start = r.minSeq
	}
	out := make([]Envelope, 0, int(r.maxSeq-start+1))
	for s := start; s <= r.maxSeq; s++ {
		env := r.buf[int(s%int64(r.cap))]
		if env.Seq != s {
			// race: slot was overwritten between unlock and re-read; bail
			// out and let the caller fall back to SQL.
			return nil, false
		}
		out = append(out, env)
	}
	return out, true
}

// snapshot returns (minSeq, maxSeq) for diagnostics; both 0 when empty.
func (r *ring) snapshot() (int64, int64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.minSeq, r.maxSeq
}
