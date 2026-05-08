package workspace

import (
	"fmt"
	"path"
)

// pathHelper centralises the on-disk layout for one namespace.
// Keeping all path math in a single struct lets the writer, reader,
// flush, and compaction code stay agnostic of the workspace root or
// namespace name they happen to be operating on, and makes it easy
// to validate the layout in tests.
type pathHelper struct {
	root string // Index-level root within the workspace
	ns   string // namespace name as supplied by the caller
}

// newPathHelper composes the per-namespace base path. An empty
// Index root collapses to ns/...; a non-empty root nests as
// root/ns/... so a single Workspace can host the index next to
// recall/, knowledge/, history/, memories/ subtrees.
func newPathHelper(root, ns string) pathHelper {
	return pathHelper{root: root, ns: ns}
}

// nsDir returns the namespace base directory.
func (p pathHelper) nsDir() string {
	if p.root == "" {
		return p.ns
	}
	return path.Join(p.root, p.ns)
}

// manifestPath / manifestTmpPath are the canonical and staging
// paths for the manifest. Writers always Write to the tmp path
// then Rename to the canonical path so readers never observe a
// partially-serialised manifest.
func (p pathHelper) manifestPath() string    { return path.Join(p.nsDir(), "manifest.json") }
func (p pathHelper) manifestTmpPath() string { return path.Join(p.nsDir(), "manifest.json.tmp") }

// walDir is the directory hosting all WAL log files for this ns.
func (p pathHelper) walDir() string { return path.Join(p.nsDir(), "wal") }

// walPath formats the WAL log path for a given sequence number.
// Sequence numbers are 1-indexed; seq 0 is reserved as the "no log"
// sentinel.
func (p pathHelper) walPath(seq uint64) string {
	return path.Join(p.walDir(), fmt.Sprintf("%016x.log", seq))
}

// segmentsDir is the parent directory of all segment subdirectories.
func (p pathHelper) segmentsDir() string { return path.Join(p.nsDir(), "segments") }

// segmentDir returns the directory for one segment id.
func (p pathHelper) segmentDir(id uint64) string {
	return path.Join(p.segmentsDir(), fmt.Sprintf("%016x", id))
}

// Per-segment file paths. meta.json is written LAST during flush
// and serves as the segment-ready commit marker: a segment whose
// directory exists but is missing meta.json is an abandoned partial
// flush and is garbage-collected on open.
func (p pathHelper) segmentMetaPath(id uint64) string {
	return path.Join(p.segmentDir(id), "meta.json")
}

func (p pathHelper) segmentDocsPath(id uint64) string {
	return path.Join(p.segmentDir(id), "docs.jsonl")
}

func (p pathHelper) segmentOffsetsPath(id uint64) string {
	return path.Join(p.segmentDir(id), "docs.offsets.bin")
}

func (p pathHelper) segmentTombstonesPath(id uint64) string {
	return path.Join(p.segmentDir(id), "tombstones.bin")
}

// File paths for the BM25 and vector sidecars are reserved here so
// every layout decision lives in one place. The segment writer in
// the current commit does not yet emit them; Stage C fills them in.
func (p pathHelper) segmentBM25Path(id uint64) string {
	return path.Join(p.segmentDir(id), "bm25.bin")
}

func (p pathHelper) segmentVectorPath(id uint64) string {
	return path.Join(p.segmentDir(id), "vector.bin")
}

// lockPath is the per-namespace lockfile honoured by the
// cross-process advisory protocol. lockTmpPath is the staging path
// used by the temp-write + Rename publication step.
func (p pathHelper) lockPath() string    { return path.Join(p.nsDir(), ".lock") }
func (p pathHelper) lockTmpPath() string { return path.Join(p.nsDir(), ".lock.tmp") }
