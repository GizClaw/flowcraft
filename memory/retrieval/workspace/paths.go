package workspace

import (
	"fmt"
	"path"
)

// pathHelper centralises the on-disk layout for one namespace.
// Keeping all path math behind one small type lets the writer, reader,
// flush, and compaction code stay agnostic of the concrete workspace layout.
type pathHelper struct {
	ns string
}

// newPathHelper composes the per-namespace base path. Workspace-level
// nesting is handled by sdk/workspace.Sub before the index is constructed.
func newPathHelper(ns string) pathHelper {
	return pathHelper{ns: ns}
}

// nsDir returns the namespace base directory.
func (p pathHelper) nsDir() string {
	return p.ns
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

// lockPath is the per-namespace lockfile honoured by the
// cross-process advisory protocol. lockTmpPath is the staging path
// used by the temp-write + Rename publication step.
func (p pathHelper) lockPath() string    { return path.Join(p.nsDir(), ".lock") }
func (p pathHelper) lockTmpPath() string { return path.Join(p.nsDir(), ".lock.tmp") }
