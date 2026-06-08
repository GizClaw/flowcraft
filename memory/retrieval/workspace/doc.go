// Package workspace implements a [retrieval.Index] backed by any
// [sdkworkspace.Workspace] implementation (MemWorkspace,
// LocalWorkspace, ScopedWorkspace, or a future remote backend such as
// S3Workspace). It is a single-process, production-grade alternative
// to the SQLite and PostgreSQL adapters for deployments that want
// every piece of agent state — recall facts, knowledge corpora,
// history archives, Memory-Tool notes — to share one sandboxed root.
//
// Despite the package name, nothing here issues raw filesystem
// syscalls; the backend's storage characteristics are determined
// entirely by the Workspace it is constructed with. Pointed at
// MemWorkspace it is an in-memory index; pointed at LocalWorkspace
// it is a durable on-disk index.
//
// # When to use this backend
//
// Pick this backend when one or more of the following apply:
//
//   - You already use a Workspace for [sdk/history] archives,
//     [sdkx/tool/memory], or knowledge ingestion, and want recall to
//     share the same root for one-knob ops/backup.
//   - You want a fully self-contained, network-free retrieval store
//     (no SQLite cgo decision, no Postgres dependency).
//   - The corpus fits the workspace medium's read-amplification budget:
//     small to mid-scale (typically < 1M docs per namespace, < 64 MB
//     per segment file).
//
// Pick the SQLite or Postgres adapters instead if any of the following
// apply:
//
//   - Multiple processes write to the same index concurrently (this
//     backend assumes a single writer; see "Concurrency" below).
//   - You need server-side richer filter pushdown beyond what
//     retrieval.Filter expresses.
//   - You need O(>1M) docs per namespace with sub-50 ms cold latency
//     (an LSM-on-files approach reads whole segments per query).
//
// # Architecture (LSM-tree on Workspace)
//
// The on-disk layout under <root>/<namespace>/ is:
//
//	manifest.json                   atomic snapshot of live segments
//	wal/<seq>.log                   append-only ops since last flush
//	segments/<id>/meta.json         doc count, vector dim, build time, checksums
//	segments/<id>/docs.jsonl        one Doc per line, immutable
//	segments/<id>/docs.offsets.bin  uint64 line→byte offsets (point lookup)
//	segments/<id>/tombstones.bin    doc IDs deleted during this segment's life
//
// Write path: every Upsert / Delete is appended to the current WAL
// log AND staged in an in-memory memtable. When the memtable crosses
// a size or count threshold it is sealed and flushed atomically:
// every segment file is written under a temporary directory, then
// the directory is renamed into segments/, then a freshly written
// manifest replaces the old one via [sdkworkspace.Workspace.Rename]
// (which the LocalWorkspace backend implements as POSIX rename(2)).
// Only after the manifest swap succeeds is the consumed WAL log
// removed. A crash between any two of these steps leaves the index
// either in the pre-flush state (manifest still points to old
// segments, WAL still present) or the post-flush state (new
// segments live, WAL gone). Both states are recoverable on startup
// without a fsck-style rebuild.
//
// Read path: a query consults the memtable plus every live segment
// listed in the manifest. Segment docs are loaded from docs.jsonl;
// BM25 token streams are derived from content on first use and cached
// in the segment reader. Search builds one global corpus across all
// live docs for each request so IDF does not depend on which segment a
// doc landed in. SparseVec is scored as a dot product against
// Doc.SparseVector. When Search receives multiple query signals, the
// backend fuses the per-signal rankings according to SearchRequest.HybridMode
// and SearchRequest.HybridOptions.
//
// Compaction: a background goroutine selects size-tiered groups of
// small segments, reads them, drops tombstoned IDs, and writes a
// merged segment using the same temp-dir + rename + manifest swap
// protocol as flush. Compaction is purely additive — readers never
// see a partial result.
//
// # Crash recovery
//
// On New() / re-open the index reads the manifest (if any), loads
// every live segment's metadata into memory, then replays each
// outstanding wal/<seq>.log into the memtable. Docs and derived BM25
// tokens are loaded lazily by segment readers. After a successful
// replay the index is open for reads and writes; the next flush will
// atomically retire those WAL files.
//
// # Concurrency
//
// This backend is single-writer. Within a single process,
// Upsert/Delete/flush/compact are serialized per namespace via an
// internal RWMutex; reads run concurrently with each other and with
// writes, observing the latest committed manifest snapshot.
// Cross-process safety relies on a best-effort lock file written by
// the active writer; a second process that opens the same root will
// notice a fresh lockfile and refuse to start (returning [ErrLocked]).
// This is a soft contract — [sdkworkspace.Workspace] exposes no
// advisory lock primitive — and operators should treat this backend
// as strictly single-writer per root.
//
// # Capabilities
//
// Retrieval capabilities are fixed by this package's index implementation:
//
//	BM25                 true
//	Vector               true
//	Sparse               true
//	Hybrid               true (multi-signal Search fused by requested HybridMode/HybridOptions)
//	FilterPushdown       true (full retrieval.Filter operator surface)
//	NativeDeleteByFilter false (implemented as scan + Delete tombstones)
//
// Storage capabilities are derived from the supplied
// [sdkworkspace.Workspace] rather than hard-coded by this package. For
// example, WriteIsAtomic, ReadAfterWrite, and Distributed reflect the
// underlying Workspace's advertised semantics, so a MemWorkspace,
// LocalWorkspace, or future remote Workspace can report different values while
// sharing the same retrieval feature set.
//
// Optional retrieval.* interfaces implemented:
// [retrieval.DocGetter], [retrieval.Filterable], [retrieval.Iterable],
// [retrieval.Countable], [retrieval.DeletableByFilter], and
// [retrieval.Droppable]. Iterate is a management/reindex scan that
// performs an O(N) live-doc walk plus ID sort. Drop is a lifecycle/admin
// operation that removes the namespace root from the backing Workspace.
package workspace
