package fact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

const (
	// hashIndexSegment names the area that maps a canonical identity to the
	// fact holding it inside one generation. It sits beside `facts` under the
	// generation, so an identity has exactly the generation's lifetime:
	// retiring the generation removes its identities with it, and a
	// replacement generation can never inherit a stale one.
	hashIndexSegment = "hashes"
	// hashIndexMarkerSegment names the single key that reports the area as
	// complete. A generation written before the area existed holds facts and
	// no marker, so the first add pays for one scan and backfills; every add
	// after that trusts an absent identity instead of rescanning.
	hashIndexMarkerSegment = "index"
	// hashIndexSchemaVersion is the current marker envelope. A marker this
	// build does not understand reads as absent: the area is backfilled
	// again rather than trusted.
	hashIndexSchemaVersion = 1
)

// hashIndexMarker is what the completeness key holds. It carries a version so
// a later change to the identity rule cannot read an older area as complete.
type hashIndexMarker struct {
	SchemaVersion int `json:"schema_version"`
}

func (store *FactStore) hashIndexPrefix(
	scope corememory.Scope, conversationID, generation string,
) (string, error) {
	prefix, err := store.generationPrefix(scope, conversationID, generation)
	if err != nil {
		return "", err
	}
	return prefix + "/" + hashIndexSegment, nil
}

// hashAreaPrefix returns the area that names every fact holding one canonical
// identity.
func (store *FactStore) hashAreaPrefix(
	scope corememory.Scope, conversationID, generation, canonicalHash string,
) (string, error) {
	prefix, err := store.hashIndexPrefix(scope, conversationID, generation)
	if err != nil {
		return "", err
	}
	return prefix + "/" + storage.EncodeSegment(canonicalHash), nil
}

func (store *FactStore) hashIndexKey(
	scope corememory.Scope, conversationID, generation, canonicalHash, factID string,
) (string, error) {
	area, err := store.hashAreaPrefix(scope, conversationID, generation, canonicalHash)
	if err != nil {
		return "", err
	}
	return area + "/" + storage.EncodeSegment(factID), nil
}

func (store *FactStore) hashIndexMarkerKey(
	scope corememory.Scope, conversationID, generation string,
) (string, error) {
	prefix, err := store.hashIndexPrefix(scope, conversationID, generation)
	if err != nil {
		return "", err
	}
	return prefix + "/" + hashIndexMarkerSegment, nil
}

// findByCanonicalHash resolves the fact of one generation holding a canonical
// identity. The identity area answers it in one key scan plus one read; a
// generation that predates the area is scanned once and backfilled, so the
// full scan is the cost of the first add to an old generation rather than the
// cost of every add.
func (store *FactStore) findByCanonicalHash(
	ctx context.Context, scope corememory.Scope, conversationID, generation, canonicalHash string,
) (persistedFact, bool, error) {
	factIDs, err := store.indexedFactIDs(ctx, scope, conversationID, generation, canonicalHash)
	if err != nil {
		return persistedFact{}, false, err
	}
	for _, factID := range factIDs {
		value, ok, err := store.read(ctx, scope, conversationID, generation, factID)
		if err != nil {
			return persistedFact{}, false, err
		}
		if !ok {
			// The entry outlived its fact: an add was interrupted between the
			// identity write and the fact write. Drop it and keep looking.
			key, err := store.hashIndexKey(scope, conversationID, generation, canonicalHash, factID)
			if err != nil {
				return persistedFact{}, false, err
			}
			if err := store.kv.Delete(ctx, key); err != nil {
				return persistedFact{}, false, fmt.Errorf("fact view: drop stale identity %q: %w", factID, err)
			}
			continue
		}
		if value.Fact.CanonicalHash == canonicalHash {
			return value, true, nil
		}
	}
	complete, err := store.hashIndexComplete(ctx, scope, conversationID, generation)
	if err != nil {
		return persistedFact{}, false, err
	}
	if complete {
		return persistedFact{}, false, nil
	}
	return store.backfillCanonicalHashes(ctx, scope, conversationID, generation, canonicalHash)
}

// indexedFactIDs returns the facts of one generation that the identity area
// names for a canonical hash, in key order. The area is probed by key: what
// the entries hold is the identity in their name, so answering a lookup reads
// no values.
func (store *FactStore) indexedFactIDs(
	ctx context.Context, scope corememory.Scope, conversationID, generation, canonicalHash string,
) ([]string, error) {
	area, err := store.hashAreaPrefix(scope, conversationID, generation, canonicalHash)
	if err != nil {
		return nil, err
	}
	keys, err := store.keysIn(ctx, area)
	if err != nil {
		return nil, fmt.Errorf("fact view: scan exact identities: %w", err)
	}
	factIDs := make([]string, 0, len(keys))
	for _, key := range keys {
		factID, err := decodeHashIndexKey(area, key)
		if err != nil {
			return nil, fmt.Errorf("fact view: decode identity key %q: %w", key, err)
		}
		factIDs = append(factIDs, factID)
	}
	return factIDs, nil
}

// hashIndexComplete reports whether the identity area of one generation has
// been populated by a build that maintains it.
func (store *FactStore) hashIndexComplete(
	ctx context.Context, scope corememory.Scope, conversationID, generation string,
) (bool, error) {
	key, err := store.hashIndexMarkerKey(scope, conversationID, generation)
	if err != nil {
		return false, err
	}
	data, err := store.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("fact view: read identity marker: %w", err)
	}
	var marker hashIndexMarker
	if err := decodeJSON(data, &marker); err != nil {
		return false, nil
	}
	return marker.SchemaVersion == hashIndexSchemaVersion, nil
}

// backfillCanonicalHashes scans a generation written before the identity area
// existed, records every fact it finds, and only then marks the area complete.
// The order is what makes an interrupted backfill safe: entries are immutable
// and idempotent, and a missing marker sends the next lookup through the same
// scan again.
func (store *FactStore) backfillCanonicalHashes(
	ctx context.Context, scope corememory.Scope, conversationID, generation, canonicalHash string,
) (persistedFact, bool, error) {
	prefix, err := store.factsPrefix(scope, conversationID, generation)
	if err != nil {
		return persistedFact{}, false, err
	}
	entries, err := store.kv.List(ctx, prefix)
	if err != nil {
		return persistedFact{}, false, fmt.Errorf("fact view: scan exact identities: %w", err)
	}
	var match persistedFact
	found := false
	for _, entry := range entries {
		factID, err := decodeFactKey(prefix, entry.Key)
		if err != nil {
			return persistedFact{}, false, err
		}
		value, ok, err := store.read(ctx, scope, conversationID, generation, factID)
		if err != nil {
			return persistedFact{}, false, err
		}
		if !ok {
			continue
		}
		if err := store.indexCanonicalHash(
			ctx, scope, conversationID, generation, value.Fact.CanonicalHash, factID); err != nil {
			return persistedFact{}, false, err
		}
		if !found && value.Fact.CanonicalHash == canonicalHash {
			match, found = value, true
		}
	}
	if err := store.markHashIndexComplete(ctx, scope, conversationID, generation); err != nil {
		return persistedFact{}, false, err
	}
	return match, found, nil
}

func (store *FactStore) markHashIndexComplete(
	ctx context.Context, scope corememory.Scope, conversationID, generation string,
) error {
	key, err := store.hashIndexMarkerKey(scope, conversationID, generation)
	if err != nil {
		return err
	}
	data, err := json.Marshal(hashIndexMarker{SchemaVersion: hashIndexSchemaVersion})
	if err != nil {
		return fmt.Errorf("fact view: encode identity marker: %w", err)
	}
	if err := store.putImmutable(ctx, key, data); err != nil {
		return fmt.Errorf("fact view: mark identity area: %w", err)
	}
	return nil
}

// indexCanonicalHash records that one fact holds one canonical identity. It is
// written before the fact itself: an entry without its fact is dropped by the
// next lookup that reads it, while a fact without its entry would be invisible
// to the area -- and the area is complete, so nothing would ever scan for it.
func (store *FactStore) indexCanonicalHash(
	ctx context.Context, scope corememory.Scope, conversationID, generation, canonicalHash, factID string,
) error {
	key, err := store.hashIndexKey(scope, conversationID, generation, canonicalHash, factID)
	if err != nil {
		return err
	}
	// The entry says everything it has to say in its name; the empty value is
	// what an area probe writes, not a nil one, which a SQL backend stores as
	// NULL and refuses.
	if err := store.putImmutable(ctx, key, []byte{}); err != nil {
		return fmt.Errorf("fact view: record identity of %q: %w", factID, err)
	}
	return nil
}

// keysIn returns the keys inside one prefix area, reading no values when the
// substrate can answer without them.
func (store *FactStore) keysIn(ctx context.Context, prefix string) ([]string, error) {
	if lister, ok := store.kv.(storage.KeyLister); ok {
		return lister.Keys(ctx, prefix)
	}
	entries, err := store.kv.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		keys = append(keys, entry.Key)
	}
	return keys, nil
}

// decodeHashIndexKey parses one key below an identity area into a fact ID.
func decodeHashIndexKey(area, key string) (string, error) {
	suffix := strings.TrimPrefix(key, area+"/")
	if suffix == key || suffix == "" || strings.Contains(suffix, "/") {
		return "", errors.New("identity key has unexpected shape")
	}
	return storage.DecodeSegment(suffix)
}
