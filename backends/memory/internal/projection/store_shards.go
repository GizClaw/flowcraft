package projection

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// Shards lists the shards one projection holds state for in one scope, in a
// stable order.
//
// The list is read from the layout rather than from a catalog: a shard exists
// because something was written under it, so the directory is the registry and
// a failed publication leaves no entry behind. It is what a global recall
// enumerates and what a rebuild compares its artifact set against, so a shard
// whose directory is there is a shard a read will find. A shard directory that
// holds no manifest names no state; readers treat it as empty and a sweep
// removes it.
func (store *Store[B, D]) Shards(ctx context.Context, scope corememory.Scope, name string) ([]string, error) {
	if err := validateAddress(scope, name); err != nil {
		return nil, err
	}
	prefix := path.Join(store.root(scope, name), "shards")
	keys, err := indexKeys(ctx, store.KV, prefix)
	if err != nil {
		return nil, fmt.Errorf("projection: list shards: %w", err)
	}
	shards := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		rest, ok := strings.CutPrefix(key, prefix+"/")
		if !ok {
			continue
		}
		segment, _, ok := strings.Cut(rest, "/")
		if !ok || segment == "" {
			continue
		}
		if _, duplicate := seen[segment]; duplicate {
			continue
		}
		shard, err := ParseShardSegment(segment)
		if err != nil {
			// A directory under shards/ that is not a shard is not this
			// protocol's: refusing names the key instead of silently
			// dropping state a later read might have served.
			return nil, fmt.Errorf("projection: decode shard key %q: %w", key, err)
		}
		seen[segment] = struct{}{}
		shards = append(shards, shard)
	}
	sort.Strings(shards)
	return shards, nil
}

// DropShard removes everything one shard holds: its manifest, its bases and its
// segments. It is what a scope-wide rebuild calls for a shard the artifact set
// no longer covers, so a conversation that was deleted stops being readable
// instead of keeping the state of its last index.
//
// The shard is unused once this returns, and the next write to it starts from a
// manifest that points at nothing.
func (store *Store[B, D]) DropShard(ctx context.Context, address Address, name string) error {
	if err := address.validate(name); err != nil {
		return err
	}
	mutex := store.lock(address, name)
	mutex.Lock()
	defer mutex.Unlock()
	root := store.shardRoot(address, name)
	keys, err := indexKeys(ctx, store.KV, root)
	if err != nil {
		return fmt.Errorf("projection: list shard objects: %w", err)
	}
	for _, key := range keys {
		if err := store.KV.Delete(ctx, key); err != nil {
			return fmt.Errorf("projection: drop shard object %q: %w", key, err)
		}
	}
	store.forgetLocked(store.residentKey(address, name))
	return nil
}

// PruneShards drops every shard of one scope that keep does not name, and
// reports the shards it dropped.
//
// A scope-wide rebuild publishes the shards its artifact set covers and leaves
// the rest exactly as they were, so a conversation deleted from the canonical
// log would keep answering queries out of its last index. Pruning after the
// publication is what makes the rebuild replace the scope instead of adding to
// it, and it runs last so a rebuild that fails halfway still leaves the state
// it did not rewrite reachable instead of deleting it first.
//
// The dropped list is for the lanes that keep a copy of a shard's table: they
// invalidate exactly the tables that stopped being reachable.
func (store *Store[B, D]) PruneShards(
	ctx context.Context,
	scope corememory.Scope,
	name string,
	keep map[string]struct{},
) ([]string, error) {
	shards, err := store.Shards(ctx, scope, name)
	if err != nil {
		return nil, err
	}
	dropped := make([]string, 0, len(shards))
	for _, shard := range shards {
		if _, kept := keep[shard]; kept {
			continue
		}
		if err := store.DropShard(ctx, Address{Scope: scope, Shard: shard}, name); err != nil {
			return dropped, err
		}
		dropped = append(dropped, shard)
	}
	return dropped, nil
}
