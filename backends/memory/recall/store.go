// Package recall persists the read path's receipts: the items a context
// request actually returned, and when.
//
// A receipt is what lets maintenance tell a fact that has kept being useful
// from one that was imported and never read. Only the retrieval provider
// writes here, and only after it has packed a result, so a path that never
// serves context -- a bulk import, a derive pass -- produces no receipt and
// cannot read as an access. A receipt is not authority: it changes nothing
// about what is retrievable, it only tells the next maintenance pass which
// items have been rehearsed.
//
// The store keeps one record per recalled item on the KV substrate. It is
// deliberately not an event log: nothing consumes individual receipts, and a
// per-item record keeps the read maintenance does -- one prefix scan per
// scope -- bounded by the scope's recalled items rather than by its recall
// history. The same logical event delivered twice is deduplicated per item:
// a record that still names the event is not counted again, which covers the
// retry case. A replay that arrives after another event has touched the item
// is no longer distinguishable from a new recall, and counts once more; that
// bound is deliberate, since exact-once would need a marker per event and
// grow without limit to protect a heuristic.
//
// The same tolerance covers two recalls of one item racing each other: both
// read the record, both write it back, and the later write wins, so one
// increment can be lost. The count is a saturating factor, not a ledger.
//
// Nothing prunes the records. One per recalled item is small, reads are by
// prefix, and a record whose item no longer exists is inert: no pass looks it
// up, and a host that wants the space back sweeps the scope's prefix.
package recall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// rootPrefix is the receipt store's storage namespace. The version is in the
// name because nothing migrates a receipt: a future format change leaves the
// old records where they are, unread and inert, and the next recalls rebuild
// the counts a pass actually needs.
const rootPrefix = "recall/v1"

// maxCount saturates an item's recall count. Counts only feed a saturating
// factor (Count/(Count + RecallSaturation)), so the cap changes no score; it
// keeps a record's size fixed no matter how long a scope runs.
const maxCount = 255

// Receipt is one item's recall record: how many context results returned it,
// when the most recent one did, and the event that last counted. LastEvent is
// what makes a retry of the same event cheap to recognize: a record that
// still names the event is skipped instead of counted twice.
type Receipt struct {
	Identity  string    `json:"identity"`
	Count     int       `json:"count"`
	Last      time.Time `json:"last,omitempty"`
	LastEvent string    `json:"last_event,omitempty"`
}

// Store records the read path's receipts on the KV substrate.
type Store struct {
	kv storage.Store
}

// NewStore builds a KV-backed receipt store.
func NewStore(kv storage.Store) (*Store, error) {
	if nilValue(kv) {
		return nil, errors.New("recall: kv store is required")
	}
	return &Store{kv: kv}, nil
}

// RecordRecall implements the retrieval provider's receipt hook: it counts
// one returned item once per event. The event is validated first, so a
// provider cannot record a half-built or unsorted event, and an event whose
// items the scope cannot address fails the write rather than dropping an
// item silently.
func (store *Store) RecordRecall(ctx context.Context, event corememory.RecallEvent) error {
	if store == nil || nilValue(store.kv) {
		return errors.New("recall: store is incomplete")
	}
	if ctx == nil {
		return errors.New("recall: context is required")
	}
	if err := event.Validate(); err != nil {
		return err
	}
	at := event.Time.UTC()
	for _, identity := range event.ItemIDs {
		key, err := itemKey(event.Scope, identity)
		if err != nil {
			return err
		}
		record, found, err := store.read(ctx, key)
		if err != nil {
			return err
		}
		if found && record.LastEvent == event.ID {
			// This event already counted on this item: a retry, not
			// another recall.
			continue
		}
		record.Identity = identity
		if record.Count < maxCount {
			record.Count++
		}
		if at.After(record.Last) {
			record.Last = at
		}
		record.LastEvent = event.ID
		if err := store.write(ctx, key, record); err != nil {
			return err
		}
	}
	return nil
}

func nilValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// Receipts returns one scope's receipts keyed by item identity. The read is
// one prefix scan, and its size follows the scope's recalled items rather
// than its recall history. A corrupt record fails the read: maintenance that
// silently skipped receipts would decay exactly the facts the caller keeps
// using, which is the failure this store exists to prevent.
func (store *Store) Receipts(ctx context.Context, scope corememory.Scope) (map[string]Receipt, error) {
	if store == nil || nilValue(store.kv) {
		return nil, errors.New("recall: store is incomplete")
	}
	if ctx == nil {
		return nil, errors.New("recall: context is required")
	}
	prefix, err := itemsPrefix(scope)
	if err != nil {
		return nil, err
	}
	entries, err := store.kv.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("recall: list receipts: %w", err)
	}
	receipts := make(map[string]Receipt, len(entries))
	for _, entry := range entries {
		var record Receipt
		if err := json.Unmarshal(entry.Value, &record); err != nil {
			return nil, fmt.Errorf("recall: decode receipt %q: %w", entry.Key, err)
		}
		if err := validateReceipt(record); err != nil {
			return nil, fmt.Errorf("recall: corrupt receipt %q: %w", entry.Key, err)
		}
		if entry.Key != prefix+"/"+record.Identity {
			return nil, fmt.Errorf("recall: receipt %q names item %q", entry.Key, record.Identity)
		}
		receipts[record.Identity] = record
	}
	return receipts, nil
}

func (store *Store) read(ctx context.Context, key string) (Receipt, bool, error) {
	data, err := store.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Receipt{}, false, nil
		}
		return Receipt{}, false, fmt.Errorf("recall: load receipt: %w", err)
	}
	var record Receipt
	if err := json.Unmarshal(data, &record); err != nil {
		return Receipt{}, false, fmt.Errorf("recall: decode receipt: %w", err)
	}
	return record, true, nil
}

func (store *Store) write(ctx context.Context, key string, record Receipt) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("recall: encode receipt %q: %w", record.Identity, err)
	}
	if err := store.kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("recall: save receipt %q: %w", record.Identity, err)
	}
	return nil
}

// itemsPrefix names the scope's receipt area. The scope partition keeps one
// scope's receipts out of another's reads and writes; the identities under it
// are opaque hard-scope-qualified names, so an item moved between scopes gets
// a different record rather than inheriting foreign counts.
func itemsPrefix(scope corememory.Scope) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return rootPrefix + "/" + partition + "/items", nil
}

func itemKey(scope corememory.Scope, identity string) (string, error) {
	if err := validateIdentity(identity); err != nil {
		return "", err
	}
	prefix, err := itemsPrefix(scope)
	if err != nil {
		return "", err
	}
	return prefix + "/" + identity, nil
}

// validateIdentity keeps one receipt one path segment. An identity that
// carried a separator could collide with another record's address, and the
// repository's identities are opaque single segments by construction.
func validateIdentity(identity string) error {
	switch {
	case strings.TrimSpace(identity) == "":
		return errors.New("recall: item identity is required")
	case identity != strings.TrimSpace(identity):
		return errors.New("recall: item identity must be trimmed")
	case strings.Contains(identity, "/"):
		return errors.New("recall: item identity must be one path segment")
	case strings.ContainsRune(identity, '\x00'):
		return errors.New("recall: item identity must not contain NUL")
	}
	return nil
}

func validateReceipt(record Receipt) error {
	if err := validateIdentity(record.Identity); err != nil {
		return err
	}
	switch {
	case record.Count <= 0 || record.Count > maxCount:
		return fmt.Errorf("recall: receipt count %d is out of range", record.Count)
	case record.Last.IsZero():
		return errors.New("recall: receipt has no recall time")
	case strings.TrimSpace(record.LastEvent) == "":
		return errors.New("recall: receipt has no event id")
	}
	return nil
}
