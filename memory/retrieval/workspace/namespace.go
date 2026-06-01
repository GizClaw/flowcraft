package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

// nsSetup orchestrates "open or create" of one namespace: it loads
// the manifest (or starts fresh), garbage-collects abandoned partial
// flushes, replays outstanding WAL logs into the memtable, and wires
// up the WAL writer. All disk I/O happens here so callers never see
// half-initialised namespace state.
func (idx *Index) ensureNamespace(ctx context.Context, name string) (*namespaceState, error) {
	if idx.closed.Load() {
		return nil, ErrClosed
	}
	if name == "" {
		return nil, errEmptyNamespace
	}

	idx.nsMu.Lock()
	if existing, ok := idx.namespaces[name]; ok {
		idx.nsMu.Unlock()
		return existing, nil
	}
	// Create+register before doing I/O so concurrent callers asking
	// for the same namespace don't all open from disk in parallel.
	st := &namespaceState{name: name}
	idx.namespaces[name] = st
	idx.nsMu.Unlock()

	st.rwMu.Lock()
	defer st.rwMu.Unlock()
	if err := idx.openNamespaceLocked(ctx, st); err != nil {
		// On open failure, evict from the map so the next caller
		// can retry instead of inheriting a broken state.
		idx.nsMu.Lock()
		delete(idx.namespaces, name)
		idx.nsMu.Unlock()
		return nil, err
	}
	return st, nil
}

// openNamespaceLocked performs the actual open. Caller holds
// st.rwMu in write mode and the namespace is *not* yet visible to
// concurrent readers.
//
// Order is: (1) acquire the cross-process lock so manifest reads
// and WAL replays below are not racing a second writer, (2) read
// manifest, (3) GC abandoned segments, (4) replay WAL into the
// memtable, (5) wire up the WAL writer, (6) start the heartbeat
// goroutine that keeps our lock alive.
func (idx *Index) openNamespaceLocked(ctx context.Context, st *namespaceState) error {
	st.paths = newPathHelper(st.name)

	lock, err := acquireLock(ctx, idx.ws, st.paths, idx.cfg.lockHeartbeat, idx.cfg.now())
	if err != nil {
		return err
	}
	st.lockHolder = lock.Holder // empty when protocol is disabled

	man, err := readManifest(ctx, idx.ws, st.paths)
	if err != nil {
		return err
	}
	if man == nil {
		man = &manifest{
			Version:    manifestVersion,
			Generation: 0,
			Segments:   nil,
			UpdatedAt:  idx.cfg.now(),
		}
	}
	st.manifest = man

	if err := gcAbandonedSegments(ctx, idx.ws, st.paths, man); err != nil {
		return err
	}

	st.memtable = newMemtable()
	highest, err := replayWAL(ctx, idx.ws, st.paths, man.FirstUnconsumedWALSeq, st.memtable)
	if err != nil {
		return err
	}
	startSeq := man.LastWALSeq
	if highest > startSeq {
		startSeq = highest
	}
	st.wal = newWALWriter(idx.ws, st.paths, idx.cfg.walMaxBytes, startSeq)

	// Heartbeat goroutine. Skipped when the protocol is disabled
	// (lockHolder=="" via acquireLock) so unsupported workspaces
	// don't spawn a no-op refresh loop.
	if st.lockHolder != "" {
		hbCtx, cancel := context.WithCancel(context.Background())
		st.lockCancel = cancel
		st.lockDone = make(chan struct{})
		go idx.runHeartbeat(hbCtx, st, st.lockHolder, st.lockDone)
	}

	return nil
}

// flushLocked seals the current memtable into a new segment, writes
// an updated manifest atomically, and retires consumed WAL files.
// Caller holds st.rwMu in write mode.
//
// flushLocked is a no-op when the memtable has no pending items.
func (idx *Index) flushLocked(ctx context.Context, st *namespaceState) error {
	if st.memtable.docCount() == 0 {
		return nil
	}

	// Rotate WAL FIRST: from this point on, new writes go to a fresh
	// log so the snapshot we are about to flush corresponds 1:1 to
	// the now-frozen previous logs.
	rotatedSeq, err := st.wal.Rotate(ctx)
	if err != nil {
		return fmt.Errorf("flush: rotate wal: %w", err)
	}

	snap := st.memtable.snapshot()
	if snap.empty() {
		return nil
	}

	segID := st.manifest.LastSegmentID + 1
	build, err := writeSegment(ctx, idx.ws, st.paths, segID, snap, idx.cfg.now())
	if err != nil {
		return fmt.Errorf("flush: write segment: %w", err)
	}

	newMan := *st.manifest
	newMan.Generation++
	newMan.LastSegmentID = segID
	newMan.UpdatedAt = idx.cfg.now()
	if rotatedSeq > 0 {
		// Everything up to and including rotatedSeq is now in the
		// new segment; the next unconsumed log is the freshly
		// rotated-into log (rotatedSeq+1).
		newMan.FirstUnconsumedWALSeq = rotatedSeq + 1
		if newMan.LastWALSeq < rotatedSeq+1 {
			newMan.LastWALSeq = rotatedSeq + 1
		}
	}
	if build != nil {
		newMan.Segments = append(newMan.Segments, build.ref)
	}

	if err := writeManifest(ctx, idx.ws, st.paths, &newMan); err != nil {
		return fmt.Errorf("flush: write manifest: %w", err)
	}
	st.manifest = &newMan

	// Best-effort WAL retirement. A failure here leaves stale logs
	// on disk; they are skipped on next open thanks to
	// FirstUnconsumedWALSeq, so correctness is preserved.
	deleteStaleWALs(ctx, idx.ws, st.paths, newMan.FirstUnconsumedWALSeq)

	// Wake the compactor: a fresh segment has just landed and the
	// new total may have crossed a merge threshold. Posted async
	// so the writer doesn't block on the worker's bookkeeping.
	idx.pokeCompactor()

	return nil
}

// readManifest returns the parsed manifest, nil if the file does
// not exist (fresh namespace), or an error wrapping [ErrCorrupt]
// when the file is present but unreadable / wrong-version.
func readManifest(ctx context.Context, ws sdkworkspace.Workspace, paths pathHelper) (*manifest, error) {
	data, err := ws.Read(ctx, paths.manifestPath())
	if err != nil {
		if errors.Is(err, sdkworkspace.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("readManifest: %w", err)
	}
	var man manifest
	if err := json.Unmarshal(data, &man); err != nil {
		return nil, fmt.Errorf("%w: manifest unmarshal: %v", ErrCorrupt, err)
	}
	if man.Version != manifestVersion {
		return nil, fmt.Errorf("%w: manifest version=%d want=%d",
			ErrCorrupt, man.Version, manifestVersion)
	}
	return &man, nil
}

// writeManifest serialises man, writes it to the staging path, then
// renames into place. The Workspace's Rename atomicity is what
// makes the swap observably-instantaneous to readers.
func writeManifest(ctx context.Context, ws sdkworkspace.Workspace, paths pathHelper, man *manifest) error {
	data, err := json.Marshal(man)
	if err != nil {
		return fmt.Errorf("writeManifest: marshal: %w", err)
	}
	if err := ws.Write(ctx, paths.manifestTmpPath(), data); err != nil {
		return fmt.Errorf("writeManifest: tmp write: %w", err)
	}
	if err := ws.Rename(ctx, paths.manifestTmpPath(), paths.manifestPath()); err != nil {
		return fmt.Errorf("writeManifest: rename: %w", err)
	}
	return nil
}

// gcAbandonedSegments removes any <ns>/segments/<id>/ directory that
// is not listed in the manifest. These are partial flushes whose
// manifest swap never completed; resurrecting them would risk
// applying half-written data.
func gcAbandonedSegments(
	ctx context.Context,
	ws sdkworkspace.Workspace,
	paths pathHelper,
	man *manifest,
) error {
	live := make(map[uint64]struct{}, len(man.Segments))
	for _, s := range man.Segments {
		live[s.ID] = struct{}{}
	}
	entries, err := ws.List(ctx, paths.segmentsDir())
	if err != nil {
		if errors.Is(err, sdkworkspace.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("gcAbandonedSegments: list: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id, err := strconv.ParseUint(e.Name(), 16, 64)
		if err != nil {
			continue
		}
		if _, ok := live[id]; ok {
			continue
		}
		// Remove abandoned segment dir; failure is logged but not
		// fatal. The orphan dir wastes disk but has no correctness
		// impact since it is not in the manifest.
		_ = ws.RemoveAll(ctx, paths.segmentDir(id))
	}
	return nil
}

// deleteStaleWALs removes wal/<seq>.log files with seq <
// firstUnconsumed. Failures are non-fatal: the logs are skipped on
// replay because their seq < FirstUnconsumedWALSeq.
func deleteStaleWALs(
	ctx context.Context,
	ws sdkworkspace.Workspace,
	paths pathHelper,
	firstUnconsumed uint64,
) {
	seqs, err := listWALSeqs(ctx, ws, paths)
	if err != nil {
		return
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for _, seq := range seqs {
		if seq >= firstUnconsumed {
			continue
		}
		_ = ws.Delete(ctx, paths.walPath(seq))
	}
}

// errEmptyNamespace is returned when the caller passes an empty
// namespace name. Categorised via errdefs so HTTP / RPC mappers
// surface a 400-class status.
var errEmptyNamespace = errdefs.Validationf("retrieval/workspace: namespace must not be empty")
