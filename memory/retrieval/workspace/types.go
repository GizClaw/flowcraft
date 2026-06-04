package workspace

import (
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// manifestVersion is the on-disk schema version of manifest.json.
// Bump when the JSON shape changes incompatibly; readers refuse to
// open a manifest with an unknown version.
const manifestVersion = 1

// segmentMetaVersion is the on-disk schema version of a segment's
// meta.json. Bump on incompatible changes; older readers refuse the
// segment, which forces a fresh ingest rather than risking silent
// corruption.
const segmentMetaVersion = 1

// walRecordVersion identifies the on-disk WAL record framing. Each
// WAL log file embeds the version once in its header; readers refuse
// unknown versions on replay.
const walRecordVersion = 1

// Sentinel errors surfaced from the backend. Each is derived from an
// [errdefs] category so HTTP / RPC adapters can map to status codes
// via [errdefs.Is*] without unwrapping by string.
var (
	// ErrLocked is returned when a second writer attempts to open a
	// namespace whose lock file is held by another process. See the
	// "Concurrency" section in the package doc. Categorised as
	// [errdefs.Conflict] because contention with another holder is
	// the canonical 409 case.
	ErrLocked = errdefs.Conflictf("retrieval/workspace: namespace is locked by another writer")

	// ErrCorrupt is returned when an on-disk file fails its
	// checksum or has an unexpected schema. The caller should treat
	// the affected namespace as unrecoverable and either restore
	// from a snapshot or re-ingest. Categorised as [errdefs.Internal]
	// because the data state is not something the caller can fix
	// by changing inputs.
	ErrCorrupt = errdefs.Internal(errdefs.Fmt("retrieval/workspace: on-disk data is corrupt"))

	// ErrClosed is returned by any operation invoked after Close.
	// Categorised as [errdefs.NotAvailable] because the resource is
	// no longer usable, but the caller can recover by reopening.
	ErrClosed = errdefs.NotAvailablef("retrieval/workspace: index is closed")

	// ErrFenced is returned when a namespace's lockfile has been
	// observed to belong to a different holder than ours, meaning
	// another writer has taken over (typically because our
	// heartbeat lapsed past the staleness window). Once fenced, a
	// namespace permanently rejects further mutations from this
	// Index instance to avoid double-write races against the new
	// holder. The caller can recover by closing the Index and
	// reopening — the new instance will acquire the lock cleanly
	// or itself observe ErrLocked.
	//
	// Categorised as [errdefs.NotAvailable] because the local
	// resource is no longer usable; like ErrClosed but specifically
	// scoped to one namespace.
	ErrFenced = errdefs.NotAvailablef("retrieval/workspace: namespace lock was taken over by another writer")
)

// lockState is the JSON payload of <ns>/.lock. The protocol is
// best-effort advisory locking: the file's content names the
// current holder and the most recent heartbeat. A second writer
// observing a heartbeat older than 3× LockHeartbeat treats the
// lock as stale and overwrites it; the now-fenced original holder
// detects the takeover on its next heartbeat tick (the Holder
// field no longer matches its own randomly-generated UUID) and
// trips ErrFenced for every subsequent operation.
//
// The protocol assumes [sdkworkspace.Workspace.Rename] is atomic
// (Capabilities.AtomicRename = true). When the underlying medium
// does not provide that — e.g. an object store — locking is
// disabled and single-writer use is the caller's responsibility;
// see the package doc.
type lockState struct {
	// Version pins the schema. Mismatches are treated as corrupt
	// and trigger a fresh acquire.
	Version int `json:"version"`

	// Holder is the random UUID identifying ONE Index instance.
	// Two acquires from the same process generate two distinct
	// holders so the protocol works even within a single binary.
	Holder string `json:"holder"`

	// PID + Hostname are diagnostic only — operators inspecting
	// the file see "who has this".
	PID      int    `json:"pid"`
	Hostname string `json:"hostname,omitempty"`

	// AcquiredAt is when the holder first wrote the file. Static
	// for the lifetime of one acquire.
	AcquiredAt time.Time `json:"acquired_at"`

	// HeartbeatAt is the most recent refresh. Updated on every
	// heartbeat tick. Stale > 3× LockHeartbeat triggers takeover.
	HeartbeatAt time.Time `json:"heartbeat_at"`
}

// lockStateVersion is the on-disk schema version of [lockState].
const lockStateVersion = 1

// manifest is the atomic snapshot of one namespace's live state.
// Written via temp-file + Rename so readers always observe a
// consistent generation. The on-disk JSON keys are lowercase.
type manifest struct {
	// Version pins the schema. Mismatches cause Open to fail rather
	// than guess at backwards-compatible decoding.
	Version int `json:"version"`

	// Generation is monotonically increasing per atomic swap. It is
	// not used for correctness but lets operators tell which copy
	// of a hand-extracted manifest was newer.
	Generation uint64 `json:"generation"`

	// Segments lists every segment that participates in reads. The
	// order is not significant.
	Segments []segmentRef `json:"segments"`

	// LastSegmentID is the highest segment id ever assigned in
	// this namespace, including ones that have since been
	// compacted away. New segment IDs use LastSegmentID + 1 to
	// keep IDs unique even across compactions.
	LastSegmentID uint64 `json:"last_segment_id"`

	// LastWALSeq is the highest WAL sequence number ever assigned.
	LastWALSeq uint64 `json:"last_wal_seq"`

	// FirstUnconsumedWALSeq is the lowest WAL sequence number whose
	// records have not yet been incorporated into a segment. On
	// replay, only wal/<seq>.log files with seq >=
	// FirstUnconsumedWALSeq are applied to the memtable; logs with
	// seq < this value are stale and may be garbage-collected. This
	// is what guarantees correctness when the writer crashes between
	// the manifest swap (which makes a new segment durable) and the
	// WAL-file deletion that would normally retire the now-redundant
	// records: the next manifest already advanced this counter, so
	// re-replaying the old log is skipped.
	FirstUnconsumedWALSeq uint64 `json:"first_unconsumed_wal_seq"`

	// UpdatedAt is the writer's local clock at the time of swap.
	// Used only for diagnostics; readers don't rely on it.
	UpdatedAt time.Time `json:"updated_at"`
}

// segmentRef is the manifest's pointer to one segment.
type segmentRef struct {
	// ID is the unique segment identifier; the segment lives at
	// segments/<id>/ inside the namespace directory.
	ID uint64 `json:"id"`

	// DocCount is the number of *live* docs at flush time, before
	// any subsequent tombstones in tombstones.bin take effect.
	DocCount int `json:"doc_count"`

	// VectorDim is the vector dimensionality, or 0 if the segment
	// holds no vectors.
	VectorDim int `json:"vector_dim"`

	// SizeBytes is the total bytes occupied by the segment files
	// (docs.jsonl + offsets + tombstones + meta), used by the
	// size-tiered compactor to pick merge candidates.
	SizeBytes int64 `json:"size_bytes"`

	// BuildAt is the writer's local clock at flush completion.
	BuildAt time.Time `json:"build_at"`
}

// segmentMeta is the per-segment header stored at meta.json. It
// duplicates a few fields from segmentRef so a segment is
// self-describing if extracted from the manifest, plus carries
// integrity hashes for the sibling files.
type segmentMeta struct {
	Version int `json:"version"`

	// ID matches segmentRef.ID.
	ID uint64 `json:"id"`

	// DocCount and VectorDim mirror segmentRef.
	DocCount  int `json:"doc_count"`
	VectorDim int `json:"vector_dim"`

	// AvgDocLength is retained for v1 metadata compatibility. It is
	// currently the average marshaled-doc byte length at segment build
	// time; Search does not use it for BM25 scoring.
	AvgDocLength float64 `json:"avg_doc_length"`

	// BuildAt is the flush completion time.
	BuildAt time.Time `json:"build_at"`

	// FileChecksums maps "docs.jsonl", "docs.offsets.bin", and
	// "tombstones.bin" to their crc32 (IEEE) of the file contents at
	// build time. Reads verify the checksum on first load and refuse
	// to use a corrupt file rather than silently feed garbled data
	// into scoring.
	FileChecksums map[string]uint32 `json:"file_checksums"`
}

// walOp identifies the kind of operation in a WAL record.
type walOp uint8

const (
	walOpUpsert walOp = 1
	walOpDelete walOp = 2
)

// walRecord is one logical entry: an Upsert (Doc carried inline) or
// a Delete (DocID only, Doc nil). The on-disk shape is JSON; binary
// length-prefixing is applied by the framing layer.
type walRecord struct {
	Op    walOp          `json:"op"`
	DocID string         `json:"doc_id,omitempty"`
	Doc   *retrieval.Doc `json:"doc,omitempty"`
}
