package projection

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// Reclaim removes every base and segment of one projection that its active
// manifest does not reference, and reports what it removed.
//
// A publication writes an immutable object and then moves the manifest at it,
// so the state a compaction or a rebuild replaced is unreachable the moment the
// new manifest lands -- but nothing ever removed it, and a lane that compacted a
// thousand times held a thousand full snapshots. That is the whole of this
// sweep: the manifest is the only pointer, so what it does not name is not
// storage, it is history.
//
// What the sweep keeps is what is readable: the active manifest's own base and
// segments. A publication that sweeps as it goes keeps one generation more --
// the one it just replaced -- so a reader that resolved the previous manifest is
// allowed to finish (see publishBaseLocked). This entry point is the sweep that
// keeps nothing but what is current, which is what a host reclaiming a workspace
// an earlier build filled wants.
//
// The sweep is scoped to one name. State under another name of the same family
// belongs to a lane generation this projection is not -- the vector lane names
// its storage after the embedding model it was built with -- and a deployment
// reading that generation still serves it; sweeping it here would be a rebuild
// silently deleting the state a rollback reads.
//
// It covers every shard of the scope, because a lane's state is the sum of its
// shards: a sweep that stopped at the shards it happened to know would leave an
// aborted publication behind in the one nobody counted.
//
// A manifest that cannot be read is refused rather than treated as empty: the
// sweep's question is what the active manifest points at, and a manifest it
// cannot decode has no answer. Nothing is deleted in that case.
func (store *Store[B, D]) Reclaim(
	ctx context.Context,
	scope corememory.Scope,
	name string,
) (component.ReclaimResult, error) {
	if err := validateAddress(scope, name); err != nil {
		return component.ReclaimResult{}, err
	}
	shards, err := store.Shards(ctx, scope, name)
	if err != nil {
		return component.ReclaimResult{}, err
	}
	var result component.ReclaimResult
	for _, shard := range shards {
		address := Address{Scope: scope, Shard: shard}
		mutex := store.lock(address, name)
		mutex.Lock()
		active, found, err := store.loadManifestLocked(ctx, address, name)
		if err != nil {
			mutex.Unlock()
			return result, err
		}
		var keep map[string]struct{}
		if found {
			keep = store.manifestReferences(address, name, active)
		}
		// A shard with no manifest is the one case where every object is
		// unreachable: a first publication that failed after writing its
		// base, or a manifest a host removed. There is nothing to keep and
		// nothing to point at.
		swept, err := store.reclaimLocked(ctx, address, name, keep)
		mutex.Unlock()
		result.Bases += swept.Bases
		result.Segments += swept.Segments
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

// reclaimLocked removes the bases and segments of one projection that keep does
// not name. The caller holds the store's lock and has read the manifest whose
// references the set holds.
//
// Only the two areas this protocol writes are candidates. A key under the lane
// root that is neither a base nor a segment is not this sweep's to remove: it
// was written by something else, and a sweep that deleted what it does not
// recognize would have to be taught every key the protocol ever gains.
func (store *Store[B, D]) reclaimLocked(
	ctx context.Context,
	address Address,
	name string,
	keep map[string]struct{},
) (component.ReclaimResult, error) {
	var result component.ReclaimResult
	root := store.shardRoot(address, name)
	keys, err := indexKeys(ctx, store.KV, root)
	if err != nil {
		return result, fmt.Errorf("projection: list stored objects: %w", err)
	}
	bases := path.Join(root, "bases") + "/"
	segments := path.Join(root, "segments") + "/"
	for _, key := range keys {
		var counter *int
		switch {
		case strings.HasPrefix(key, bases):
			counter = &result.Bases
		case strings.HasPrefix(key, segments):
			counter = &result.Segments
		default:
			continue
		}
		if _, referenced := keep[key]; referenced {
			continue
		}
		if err := store.KV.Delete(ctx, key); err != nil {
			return result, fmt.Errorf("projection: reclaim object %q: %w", key, err)
		}
		*counter++
	}
	return result, nil
}

// manifestReferences is the set of object keys a manifest points at: its base
// and the segments chained after it. A manifest that holds nothing names
// nothing, and the set is empty rather than nil so a caller can add to it.
func (store *Store[B, D]) manifestReferences(
	address Address,
	name string,
	active Manifest,
) map[string]struct{} {
	references := make(map[string]struct{}, 1+len(active.Segments))
	if active.Base != nil {
		references[store.buildPath(address, name, active.Base.ID)] = struct{}{}
	}
	for _, ref := range active.Segments {
		references[store.segmentPath(address, name, ref.ID)] = struct{}{}
	}
	return references
}

// sweepSupersededLocked is the publication-time sweep: after current is
// published, the objects it names are what a read resolves, and the objects
// previous names are the only other state the lane ever served. Those are kept
// for one publication of grace -- a reader that resolved the previous manifest
// can still materialize it -- and the next publication drops them, so the lane
// holds two generations at most. The grace generation costs no disk a
// compaction does not already hold: it is the state the publication just read
// to build what replaced it.
//
// Everything else under the lane's two object areas was already unreachable
// before this publication: an earlier compaction's grace generation, or a base
// a failed publication wrote and never pointed at.
//
// A sweep that fails is not a failed write: the publication is durable and the
// objects it left behind are collected by the next one, or by Reclaim. The
// error is therefore reported to nobody, and what it would have removed stays
// stored until a sweep succeeds.
func (store *Store[B, D]) sweepSupersededLocked(
	ctx context.Context,
	address Address,
	name string,
	current Manifest,
	previous Manifest,
	previousFound bool,
) {
	keep := store.manifestReferences(address, name, current)
	if previousFound {
		for key := range store.manifestReferences(address, name, previous) {
			keep[key] = struct{}{}
		}
	}
	_, _ = store.reclaimLocked(ctx, address, name, keep)
}
