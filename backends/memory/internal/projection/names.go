package projection

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// OtherNames reports the storage names this store's family holds state under
// for one scope, excluding exclude. It is how a lane whose storage name is a
// function of its producer identity -- the vector lane forks one name per
// generation -- says that the state an earlier producer wrote is still on disk
// while the name the running lane writes holds nothing: the manifest of the
// name it writes cannot report a mismatch, because there is nothing there to
// disagree with.
//
// The scan reads keys and no values (a substrate that cannot do that falls back
// to List), so it costs one entry per key the family has written under the
// scope rather than the state those keys hold, and the names come back sorted.
func (store *Store[B, D]) OtherNames(ctx context.Context, scope corememory.Scope, exclude string) ([]string, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	prefix := store.indexesPrefix(scope)
	keys, err := indexKeys(ctx, store.KV, prefix)
	if err != nil {
		return nil, fmt.Errorf("projection: list indexes: %w", err)
	}
	seen := make(map[string]struct{}, len(keys))
	names := make([]string, 0, len(keys))
	for _, key := range keys {
		name, err := decodeIndexName(prefix, key)
		if err != nil {
			return nil, fmt.Errorf("projection: decode index key %q: %w", key, err)
		}
		if name == exclude {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// indexKeys reads the keys under one index area. The scan is key-only when the
// substrate offers it: the area holds every base and segment of every name the
// family ever wrote, so a value-carrying list would read the whole stored state
// of the scope to answer a question about its shape.
func indexKeys(ctx context.Context, kv storage.Store, prefix string) ([]string, error) {
	if lister, ok := kv.(storage.KeyLister); ok {
		return lister.Keys(ctx, prefix)
	}
	entries, err := kv.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		keys = append(keys, entry.Key)
	}
	return keys, nil
}

// decodeIndexName reads the index name back out of one key below an indexes
// prefix: the key's first segment under the prefix, which the store wrote with
// encode. A key without one is not a key this family wrote.
func decodeIndexName(prefix, key string) (string, error) {
	rest, ok := strings.CutPrefix(key, prefix+"/")
	if !ok {
		return "", errors.New("key is outside the indexes prefix")
	}
	segment, _, ok := strings.Cut(rest, "/")
	if !ok || segment == "" {
		return "", errors.New("key names no index")
	}
	encoded, ok := strings.CutPrefix(segment, "k_")
	if !ok {
		return "", errors.New("key segment is not an encoded name")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}
