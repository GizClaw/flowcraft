package workspace

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/memory/retrieval"
)

// Upsert stages a batch of documents in the named namespace's
// memtable, durably appending each to the WAL first so a crash
// before the next flush still leaves the operations recoverable.
// When the memtable crosses the configured doc-count or byte-size
// threshold, Upsert flushes synchronously before returning so the
// caller can rely on bounded memory usage.
//
// Documents are upsert-overwrite by ID: a later Upsert of the same
// ID supersedes the earlier one. Empty IDs are rejected as a hard
// validation error rather than silently coalesced.
func (idx *Index) Upsert(ctx context.Context, namespace string, docs []retrieval.Doc) error {
	if idx.closed.Load() {
		return ErrClosed
	}
	if len(docs) == 0 {
		return nil
	}
	for i, d := range docs {
		if d.ID == "" {
			return fmt.Errorf("Upsert[%d]: doc.ID must not be empty", i)
		}
	}

	st, err := idx.ensureNamespace(ctx, namespace)
	if err != nil {
		return err
	}
	if err := fenceCheck(st); err != nil {
		return err
	}

	st.rwMu.Lock()
	defer st.rwMu.Unlock()

	for _, d := range docs {
		// Snapshot a marshaled byte estimate; cheaper to compute
		// once and reuse for both WAL append and memtable book-
		// keeping than to remarshal twice.
		raw, err := json.Marshal(d)
		if err != nil {
			return fmt.Errorf("Upsert: marshal %s: %w", d.ID, err)
		}
		rec := walRecord{Op: walOpUpsert, DocID: d.ID, Doc: &d}
		if err := st.wal.Append(ctx, rec); err != nil {
			return err
		}
		st.memtable.upsert(d, len(raw))
	}

	return idx.maybeFlushLocked(ctx, st)
}

// Delete removes documents by ID. Like Upsert, the operation is
// WAL-durable before returning, and triggers a synchronous flush
// when memtable thresholds are crossed. Deleting an unknown ID is
// a no-op (matching every other retrieval.Index implementation).
func (idx *Index) Delete(ctx context.Context, namespace string, ids []string) error {
	if idx.closed.Load() {
		return ErrClosed
	}
	if len(ids) == 0 {
		return nil
	}

	st, err := idx.ensureNamespace(ctx, namespace)
	if err != nil {
		return err
	}
	if err := fenceCheck(st); err != nil {
		return err
	}

	st.rwMu.Lock()
	defer st.rwMu.Unlock()

	for _, id := range ids {
		if id == "" {
			continue
		}
		rec := walRecord{Op: walOpDelete, DocID: id}
		if err := st.wal.Append(ctx, rec); err != nil {
			return err
		}
		st.memtable.remove(id)
	}

	return idx.maybeFlushLocked(ctx, st)
}

// maybeFlushLocked triggers a flush if memtable thresholds are
// crossed. Caller holds st.rwMu in write mode.
func (idx *Index) maybeFlushLocked(ctx context.Context, st *namespaceState) error {
	if idx.cfg.memtableMaxDocs > 0 &&
		st.memtable.docCount() >= idx.cfg.memtableMaxDocs {
		return idx.flushLocked(ctx, st)
	}
	if idx.cfg.memtableMaxBytes > 0 &&
		st.memtable.approxByteCount() >= idx.cfg.memtableMaxBytes {
		return idx.flushLocked(ctx, st)
	}
	return nil
}

// Flush forces a synchronous flush of the named namespace's
// memtable, regardless of thresholds. Useful for tests, for
// graceful shutdown ahead of [Close], and for callers that want
// "everything currently buffered is now in a segment" semantics
// (for example before reading a backup).
//
// Flush on a non-existent or empty namespace is a no-op.
func (idx *Index) Flush(ctx context.Context, namespace string) error {
	if idx.closed.Load() {
		return ErrClosed
	}
	st, err := idx.ensureNamespace(ctx, namespace)
	if err != nil {
		return err
	}
	if err := fenceCheck(st); err != nil {
		return err
	}
	st.rwMu.Lock()
	defer st.rwMu.Unlock()
	return idx.flushLocked(ctx, st)
}
