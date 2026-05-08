package workspace

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// Compaction
//
// The workspace backend follows a simple size-tiered LSM compaction
// policy. Every Search opens one segmentReader per live segment in
// the manifest and merges results in memory; if segments are left
// to accumulate at the rate flushes produce them the per-Search
// fan-out becomes the dominant cost. Compaction collapses small
// peers into larger ones to keep that fan-out bounded.
//
// Selection (size-tiered)
//
//   - Bucket every live segment by floor(log2(SizeBytes)). All
//     segments smaller than ~1 KiB share bucket 0.
//   - Drop segments larger than [Config.compactMaxSize] from
//     consideration; they have already paid their last write
//     amplification.
//   - The first bucket that holds at least [Config.compactMin]
//     candidates is the merge group. We merge the OLDEST
//     compactMin segments (lowest IDs) — same shape as
//     RocksDB's universal compaction.
//
// Merge invariants
//
//   - Source segments are immutable, so picking + reading them
//     does not require any namespace lock.
//   - Across the picked group: for each doc ID the NEWEST source
//     segment wins (highest segment ID). A tombstone in any source
//     suppresses every older source's copy.
//   - When the group includes the segment with the lowest ID in
//     the namespace, all tombstones in the group are FINAL and
//     can be dropped from the destination. If the group does not
//     include the oldest segment, tombstones must be carried
//     forward in case a not-yet-merged older segment still holds
//     the doc.
//
// Atomicity
//
//   - The destination segment is written under a fresh ID
//     (manifest.LastSegmentID+1) using the same meta-last commit
//     protocol as flush.
//   - The manifest swap holds [namespaceState.rwMu] briefly: read
//     the latest manifest, drop the merged segment refs, append
//     the new ref, bump generation, write tmp+rename. Concurrent
//     flushes that landed during the merge are preserved because
//     we filter by exact ID match rather than overwrite Segments
//     wholesale.
//   - Source segment directories are removed AFTER the manifest
//     swap. A crash between swap and retire leaves orphans; the
//     next ensureNamespace's gcAbandonedSegments removes them.
//
// Concurrency vs reads / writes
//
//   - Compaction holds [namespaceState.compactMu] for its entire
//     run so two compactions on the same namespace cannot collide.
//   - Reads run concurrently with the merge: their open
//     segmentReaders reference live, immutable bytes regardless
//     of the manifest swap. Read paths that look up the manifest
//     after the swap see the new layout; older readers continue
//     using their snapshot until the surrounding handle is
//     released.
//   - Writes (Upsert, Delete, Flush) only contend at the brief
//     manifest-swap window when compaction takes rwMu. Otherwise
//     the merge runs entirely outside rwMu.

// compactionLoop drives the background size-tiered compactor.
// Wakes on either a periodic tick or a flush poke; on each wake
// scans every namespace and runs compaction for any namespace whose
// current state exceeds the merge threshold.
//
// The loop owns the supplied ctx; cancelling it is the only way to
// stop the loop, and is exactly what [Index.Close] does.
func (idx *Index) compactionLoop(ctx context.Context) {
	defer close(idx.compactDone)

	tick := time.NewTicker(idx.cfg.compactInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-idx.compactWake:
		}
		idx.runCompactionRound(ctx)
	}
}

// runCompactionRound scans every namespace and runs at most one
// compaction per namespace per round. A "fast namespace" cannot
// starve others because the loop ticks again immediately after
// the round finishes.
func (idx *Index) runCompactionRound(ctx context.Context) {
	idx.nsMu.Lock()
	states := make([]*namespaceState, 0, len(idx.namespaces))
	for _, st := range idx.namespaces {
		states = append(states, st)
	}
	idx.nsMu.Unlock()

	for _, st := range states {
		if ctx.Err() != nil {
			return
		}
		// Skip fenced namespaces: their writer was taken over,
		// and any compaction we run from this Index would race
		// the new holder's manifest swaps.
		if st.fenced.Load() {
			continue
		}
		// One compaction at a time per namespace; skip if another
		// caller (e.g. an explicit Index.Compact call) holds it.
		if !st.compactMu.TryLock() {
			continue
		}
		_ = idx.compactNamespaceLocked(ctx, st)
		st.compactMu.Unlock()
	}
}

// Compact forces one compaction round on namespace, regardless of
// the auto-compactor's schedule. Safe to call concurrently with
// the background worker — the per-namespace compactMu serialises
// them. Returns nil for an unknown / empty / already-tidy
// namespace.
//
// Useful for tests, for shutdown sequences that want to leave the
// store in compact form, and for workloads that have disabled
// autoCompact via [WithAutoCompact](false).
func (idx *Index) Compact(ctx context.Context, namespace string) error {
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
	st.compactMu.Lock()
	defer st.compactMu.Unlock()
	return idx.compactNamespaceLocked(ctx, st)
}

// compactNamespaceLocked runs one compaction step. Caller holds
// st.compactMu. May be a no-op when no bucket meets the threshold.
func (idx *Index) compactNamespaceLocked(ctx context.Context, st *namespaceState) error {
	// Snapshot the manifest under rwMu so the picker sees a
	// consistent view; the actual merge runs OUTSIDE rwMu.
	st.rwMu.RLock()
	man := st.manifest
	rwReadHeld := man != nil
	if !rwReadHeld {
		st.rwMu.RUnlock()
		return nil
	}
	manifestCopy := *man
	manifestCopy.Segments = append([]segmentRef(nil), man.Segments...)
	st.rwMu.RUnlock()

	group := pickCompactionGroup(manifestCopy.Segments, idx.cfg.compactMin, idx.cfg.compactMaxSize)
	if len(group) < idx.cfg.compactMin {
		return nil
	}

	// Decide whether the group is "tail-anchored": includes the
	// oldest live segment overall. If so, tombstones whose
	// referent doc is not in any merge source can be discarded
	// because there is no older segment that might still hold the
	// doc body.
	tailAnchored := isTailAnchored(manifestCopy.Segments, group)

	merged, err := mergeSegments(ctx, idx, st, group, tailAnchored)
	if err != nil {
		return err
	}

	// Manifest swap: re-read latest manifest under rwMu and
	// transactionally remove merged refs / append the new ref.
	st.rwMu.Lock()
	defer st.rwMu.Unlock()
	if st.manifest == nil {
		// Namespace was reset between picker and swap; abandon
		// the dest segment, gcAbandonedSegments will retire it.
		return nil
	}
	mergedIDs := make(map[uint64]struct{}, len(group))
	for _, r := range group {
		mergedIDs[r.ID] = struct{}{}
	}
	newSegs := make([]segmentRef, 0, len(st.manifest.Segments)-len(group)+1)
	for _, r := range st.manifest.Segments {
		if _, ok := mergedIDs[r.ID]; ok {
			continue
		}
		newSegs = append(newSegs, r)
	}
	if merged != nil {
		newSegs = append(newSegs, *merged)
	}
	newMan := *st.manifest
	newMan.Generation++
	newMan.Segments = newSegs
	if merged != nil && newMan.LastSegmentID < merged.ID {
		newMan.LastSegmentID = merged.ID
	}
	newMan.UpdatedAt = idx.cfg.now()
	if err := writeManifest(ctx, idx.ws, st.paths, &newMan); err != nil {
		return fmt.Errorf("compact: write manifest: %w", err)
	}
	st.manifest = &newMan

	// Best-effort retirement of source segment directories. Crash
	// here leaves orphans which the next ensureNamespace removes.
	for _, r := range group {
		_ = idx.ws.RemoveAll(ctx, st.paths.segmentDir(r.ID))
	}
	return nil
}

// pickCompactionGroup is the size-tiered / universal selector.
//
// Strategy: sort live segments by ID (oldest first), exclude any
// already over [Config.compactMaxSize], then take the OLDEST
// compactMin contiguous-by-ID candidates whose pairwise size ratio
// stays inside maxRatio (default 4×). The ratio gate keeps the
// merge from dragging a 100 MiB segment together with a few KB of
// tombstones — that would amplify writes for negligible read-side
// benefit. Tombstone-only segments are tiny and their immediate
// older sibling is usually the one that holds the deleted doc, so
// in practice the gate still pairs them up.
//
// Returns nil when no eligible window exists.
func pickCompactionGroup(segs []segmentRef, minSegments int, maxSize int64) []segmentRef {
	const maxRatio = 4.0

	if minSegments < 2 {
		minSegments = 2
	}
	eligible := make([]segmentRef, 0, len(segs))
	for _, s := range segs {
		if maxSize > 0 && s.SizeBytes > maxSize {
			continue
		}
		eligible = append(eligible, s)
	}
	if len(eligible) < minSegments {
		return nil
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].ID < eligible[j].ID })

	// Slide a window of size minSegments and accept the first that
	// fits the ratio constraint. Walking oldest-first means
	// tombstone consolidation (a tail-anchored merge) tends to
	// happen as soon as the threshold is hit.
	for start := 0; start+minSegments <= len(eligible); start++ {
		win := eligible[start : start+minSegments]
		var minSz, maxSz int64 = math.MaxInt64, 0
		for _, w := range win {
			sz := w.SizeBytes
			if sz < 1 {
				sz = 1
			}
			if sz < minSz {
				minSz = sz
			}
			if sz > maxSz {
				maxSz = sz
			}
		}
		if minSz <= 0 || float64(maxSz)/float64(minSz) <= maxRatio {
			out := make([]segmentRef, len(win))
			copy(out, win)
			return out
		}
	}
	return nil
}

// isTailAnchored reports whether group includes the oldest live
// segment in segs. When true, tombstones in the group can be
// discarded because no still-live older segment can resurrect the
// deleted IDs.
func isTailAnchored(segs, group []segmentRef) bool {
	if len(segs) == 0 || len(group) == 0 {
		return false
	}
	minID := segs[0].ID
	for _, s := range segs[1:] {
		if s.ID < minID {
			minID = s.ID
		}
	}
	for _, g := range group {
		if g.ID == minID {
			return true
		}
	}
	return false
}

// mergeSegments reads every source segment, deduplicates by doc ID
// (newest-segment wins), applies tombstones, optionally drops them
// when the group is tail-anchored, and writes the result as one
// new segment. Returns the segmentRef to record in the manifest;
// returns nil with no error when the merged result is empty
// (everything was tombstoned away in a tail-anchored group).
func mergeSegments(
	ctx context.Context,
	idx *Index,
	st *namespaceState,
	group []segmentRef,
	tailAnchored bool,
) (*segmentRef, error) {
	// Process group OLDEST -> NEWEST. We track per-ID the freshest
	// state seen so far: a later (newer) tombstone overrides an
	// earlier upsert; a later upsert overrides an earlier
	// tombstone.
	sorted := append([]segmentRef(nil), group...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	type entry struct {
		doc      retrieval.Doc
		isTomb   bool
		fromGrp  bool // always true here; kept for clarity
		segOrder int  // index in sorted (higher = newer)
	}
	state := make(map[string]*entry)

	for i, ref := range sorted {
		seg, err := openSegmentReader(ctx, idx.ws, st.paths, ref)
		if err != nil {
			return nil, fmt.Errorf("compact merge: open seg %x: %w", ref.ID, err)
		}
		// Tombstones first: they delete any earlier doc state we
		// might have recorded for these IDs (within this merge).
		for tombID := range seg.tombSet {
			if e, ok := state[tombID]; ok && e.segOrder >= i {
				// Already-newer state from this same segment
				// shouldn't happen (one segment never both
				// upserts and tombstones the same ID inside the
				// same flush), but guard anyway.
				continue
			}
			state[tombID] = &entry{isTomb: true, fromGrp: true, segOrder: i}
		}
		if err := seg.loadDocs(ctx); err != nil {
			return nil, err
		}
		for _, d := range seg.docs {
			state[d.ID] = &entry{doc: d, fromGrp: true, segOrder: i}
		}
	}

	upserts := make([]retrieval.Doc, 0, len(state))
	tombs := make([]string, 0)
	for id, e := range state {
		if e.isTomb {
			if !tailAnchored {
				tombs = append(tombs, id)
			}
			continue
		}
		upserts = append(upserts, e.doc)
	}

	if len(upserts) == 0 && len(tombs) == 0 {
		// Tail-anchored merge of an all-tombstone group: nothing
		// to record; manifest swap will simply remove the merged
		// segments.
		return nil, nil
	}

	// Allocate the destination ID against the latest manifest so
	// it is unique even if a flush has landed between the picker
	// and now.
	st.rwMu.RLock()
	destID := st.manifest.LastSegmentID + 1
	st.rwMu.RUnlock()

	snap := &memtableSnapshot{}
	for _, d := range upserts {
		snap.items = append(snap.items, memtableItem{op: walOpUpsert, id: d.ID, doc: cloneDocPtr(d)})
	}
	for _, id := range tombs {
		snap.items = append(snap.items, memtableItem{op: walOpDelete, id: id})
	}

	build, err := writeSegment(ctx, idx.ws, st.paths, destID, snap, idx.cfg.now())
	if err != nil {
		return nil, fmt.Errorf("compact merge: write seg %x: %w", destID, err)
	}
	if build == nil {
		return nil, nil
	}
	return &build.ref, nil
}

// cloneDocPtr returns a heap-allocated clone, suitable for
// memtableItem.doc which expects a *Doc.
func cloneDocPtr(d retrieval.Doc) *retrieval.Doc {
	c := cloneDoc(d)
	return &c
}
