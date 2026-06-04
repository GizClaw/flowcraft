package workspace

import (
	"sync"

	"github.com/GizClaw/flowcraft/memory/retrieval"
)

// memtable is the in-memory staging buffer that absorbs Upsert /
// Delete batches between flushes. Insertion order is preserved so
// flush emits docs in deterministic order; per-doc dedup collapses
// repeated upserts of the same ID to the latest version, matching
// the user-facing semantics of an upsert-overwrite store.
//
// memtable is not goroutine-safe on its own; the namespace-level
// rwMu serialises access. Reads use [memtable.snapshot] to obtain a
// stable view that can outlive a concurrent reset+flush cycle.
type memtable struct {
	mu sync.Mutex

	// items preserves insertion order. A subsequent Upsert/Delete
	// of an already-staged ID overwrites the entry in place
	// (tracked via index) so the buffer never grows unboundedly
	// from doc churn alone.
	items []memtableItem

	// index maps doc_id -> position in items, enabling O(1) dedup.
	index map[string]int

	// approxBytes accumulates a cheap upper bound on the JSON byte
	// size of staged docs — used to drive the byte-budget flush
	// trigger without re-marshaling on every Upsert.
	approxBytes int
}

// memtableItem is one staged operation. For Upsert, doc is non-nil
// and op == walOpUpsert. For Delete, doc is nil and op ==
// walOpDelete (the buffered tombstone is what merges with reads
// over older segments).
type memtableItem struct {
	op  walOp
	id  string
	doc *retrieval.Doc
}

// newMemtable returns an empty memtable.
func newMemtable() *memtable {
	return &memtable{index: make(map[string]int)}
}

// upsert stages a doc, replacing any previously staged entry for the
// same ID. sizeHint is the approximate JSON byte size of d; callers
// pass the post-marshal length when convenient or a conservative
// upper bound otherwise.
func (m *memtable) upsert(d retrieval.Doc, sizeHint int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	clone := cloneDoc(d)
	item := memtableItem{op: walOpUpsert, id: d.ID, doc: &clone}
	if i, ok := m.index[d.ID]; ok {
		// Overwriting an existing entry; bytes ≈ replace, not add.
		// We don't track per-entry bytes precisely, so just bump by
		// the sizeHint delta vs a conservative previous estimate.
		m.items[i] = item
		m.approxBytes += sizeHint
		return
	}
	m.index[d.ID] = len(m.items)
	m.items = append(m.items, item)
	m.approxBytes += sizeHint
}

// remove stages a tombstone for id. Subsequent upserts of the same
// id replace the tombstone; a read against the memtable sees the
// latest staged op as the truth.
func (m *memtable) remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item := memtableItem{op: walOpDelete, id: id}
	if i, ok := m.index[id]; ok {
		m.items[i] = item
		return
	}
	m.index[id] = len(m.items)
	m.items = append(m.items, item)
}

// applyWALRecord ingests a record produced by replayWAL. Identical
// semantics to upsert / remove except no size accounting (the
// bytes-budget trigger is irrelevant during recovery).
func (m *memtable) applyWALRecord(rec walRecord) {
	switch rec.Op {
	case walOpUpsert:
		if rec.Doc == nil {
			return
		}
		m.upsert(*rec.Doc, 0)
	case walOpDelete:
		if rec.DocID == "" {
			return
		}
		m.remove(rec.DocID)
	}
}

// docCount returns the number of distinct IDs staged (counting
// tombstones).
func (m *memtable) docCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}

// approxByteCount returns the running byte estimate.
func (m *memtable) approxByteCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.approxBytes
}

// snapshot returns a copied, sealed view of the current items and
// resets the memtable to empty. Used by flush: the snapshot is
// written out under the namespace's write lock, then the lock is
// released — readers picking up the new manifest see the resulting
// segment, and writers continue staging into the now-empty memtable
// without contention with the in-flight flush serialisation.
func (m *memtable) snapshot() *memtableSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap := &memtableSnapshot{items: m.items, approxBytes: m.approxBytes}
	m.items = nil
	m.index = make(map[string]int)
	m.approxBytes = 0
	return snap
}

// restoreSnapshot puts a failed flush snapshot back into the live
// memtable. Caller holds the namespace write lock, so no newer writes
// can have been staged since snapshot() emptied the memtable.
func (m *memtable) restoreSnapshot(snap *memtableSnapshot) {
	if snap == nil || snap.empty() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items = append([]memtableItem(nil), snap.items...)
	m.index = make(map[string]int, len(m.items))
	m.approxBytes = snap.approxBytes
	for i, it := range m.items {
		m.index[it.id] = i
	}
}

// memtableSnapshot is the sealed view returned by [memtable.snapshot].
type memtableSnapshot struct {
	items       []memtableItem
	approxBytes int
}

// upserts iterates docs that should be persisted to the new segment
// (i.e., latest op per ID is upsert). Used by the segment writer.
func (s *memtableSnapshot) upserts() []retrieval.Doc {
	out := make([]retrieval.Doc, 0, len(s.items))
	for _, it := range s.items {
		if it.op == walOpUpsert && it.doc != nil {
			out = append(out, *it.doc)
		}
	}
	return out
}

// tombstones iterates IDs whose latest staged op is a delete.
func (s *memtableSnapshot) tombstones() []string {
	out := make([]string, 0)
	for _, it := range s.items {
		if it.op == walOpDelete {
			out = append(out, it.id)
		}
	}
	return out
}

// empty reports whether snapshot has no operations to flush.
func (s *memtableSnapshot) empty() bool {
	return len(s.items) == 0
}
