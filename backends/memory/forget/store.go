// Package forget persists the soft-forget ledger: the items one scope's read
// path is told not to serve.
//
// A forget is an overlay, not a removal. The canonical messages, the derived
// facts, the summary records, and the projection lanes keep everything they
// held, and the assembly's own store reads still see all of it; what changes is
// that a context request does not serve the named items, and a parent chain
// stops where one of them sits. That is what makes a forget the primitive a
// host can offer for a request it has to be able to take back -- restoring an
// item is deleting its ledger entry, and nothing has to be re-derived. It is
// also where the limits come from: unreachable is not unrecoverable, and a
// record that quotes a forgotten item -- a summary over the facts of a
// conversation, a document section over its chunks -- is a different address,
// which stays servable unless the host forgets it too. The ledger is what makes
// that auditable: it names every address the read path was told to drop.
//
// The store keeps one record per forgotten item on the KV substrate, addressed
// by the item's hard-scope-qualified identity
// (corememory.ContextAddress.Identity). One prefix scan answers a whole
// request, and its size follows the forgotten set rather than the scope: the
// set holds the items a host decided to forget, not the items a scope has
// judged. The identity is also what makes a forget outlast a re-derivation: a
// fact id is derived from the fact's content, so the fact a later policy
// derives again lands on the identity that is already forgotten.
package forget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// rootPrefix is the ledger's storage namespace. The version is in the name
// because nothing migrates a ledger: a future format change leaves the old
// records where they are, unread and inert, and a host that needs them re-hides
// from its own record of the request.
const rootPrefix = "forget/v1"

// maxReasonBytes bounds one record's reason. The reason is the host's own note
// -- a request id, a ticket, a policy name -- so it is kept as written and
// capped rather than parsed.
const maxReasonBytes = 512

// Record is one ledger entry: the item a forget covers, why it was forgotten,
// and when.
type Record struct {
	// Identity is the hard-scope-qualified name the read path looks up.
	Identity string `json:"identity"`
	// Address is the item the identity was derived from: what a host reads
	// back to know what it forgot.
	Address corememory.ContextAddress `json:"address"`
	// Reason is the host's note for the forget, as it was passed in.
	Reason string `json:"reason,omitempty"`
	// HiddenAt is when the item was first forgotten. A later forget of the
	// same item keeps this time: the ledger states since when an item has
	// been unreachable, not when it was last asked about.
	HiddenAt time.Time `json:"hidden_at"`
}

// Diagnostics reports one scope's ledger, without reading the records
// themselves: how many items the read path is told to drop.
type Diagnostics struct {
	Items int `json:"items"`
}

// Store persists one scope's forgotten items on the KV substrate.
type Store struct {
	kv    storage.Store
	clock func() time.Time
}

// Option configures the ledger store.
type Option func(*Store)

// WithClock replaces the store's clock, which stamps HiddenAt.
func WithClock(clock func() time.Time) Option {
	return func(store *Store) {
		if clock != nil {
			store.clock = clock
		}
	}
}

// NewStore builds a KV-backed ledger store.
func NewStore(kv storage.Store, options ...Option) (*Store, error) {
	if nilValue(kv) {
		return nil, errors.New("forget: kv store is required")
	}
	store := &Store{kv: kv, clock: time.Now}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	return store, nil
}

// Forget records the addresses as forgotten in one scope, and returns the
// records that now cover them. An address that is already recorded keeps the
// record it has -- a retry or a repeated request writes nothing, and the ledger
// keeps saying since when the item has been forgotten and why it first was.
// The addresses are deduplicated in first-appearance order, since two
// addresses name one identity at most once.
//
// Every address is resolved before the first write, so a call that names an
// address the ledger cannot address records nothing rather than recording the
// addresses that came before it. A storage failure mid-call is the other case,
// and it is retryable: the records that landed stay landed, and naming the same
// addresses again completes the rest.
func (store *Store) Forget(
	ctx context.Context,
	scope corememory.Scope,
	addresses []corememory.ContextAddress,
	reason string,
) ([]Record, error) {
	if store == nil || nilValue(store.kv) {
		return nil, errors.New("forget: store is incomplete")
	}
	if ctx == nil {
		return nil, errors.New("forget: context is required")
	}
	if err := validateReason(reason); err != nil {
		return nil, err
	}
	entries, err := plan(scope, addresses)
	if err != nil {
		return nil, err
	}
	at := store.clock().UTC()
	if at.IsZero() {
		return nil, errors.New("forget: clock returned zero time")
	}
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		current, found, err := store.read(ctx, scope, entry.key)
		if err != nil {
			return nil, err
		}
		if found {
			records = append(records, current)
			continue
		}
		record := Record{Identity: entry.identity, Address: entry.address, Reason: reason, HiddenAt: at}
		if err := store.write(ctx, entry.key, record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

// Restore removes the named addresses from the ledger, and returns the records
// it removed in the order they were named. An address that is not forgotten is
// not an error: the caller asked for a state, and it holds. The addresses are
// deduplicated the way Forget deduplicates them.
//
// Restoring is what makes a forget soft: the item's record was the only thing
// that made the read path drop it, so the item is servable again as soon as the
// record is gone. It is also why Restore takes identities seriously -- it
// deletes exactly what the caller named, and reports what it deleted rather
// than counting.
func (store *Store) Restore(
	ctx context.Context,
	scope corememory.Scope,
	addresses []corememory.ContextAddress,
) ([]Record, error) {
	if store == nil || nilValue(store.kv) {
		return nil, errors.New("forget: store is incomplete")
	}
	if ctx == nil {
		return nil, errors.New("forget: context is required")
	}
	entries, err := plan(scope, addresses)
	if err != nil {
		return nil, err
	}
	removed := make([]Record, 0, len(entries))
	for _, entry := range entries {
		current, found, err := store.read(ctx, scope, entry.key)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if err := store.kv.Delete(ctx, entry.key); err != nil {
			return nil, fmt.Errorf("forget: restore %q: %w", entry.identity, err)
		}
		removed = append(removed, current)
	}
	return removed, nil
}

// Forgotten returns one scope's ledger, oldest first, optionally narrowed to
// one conversation. An empty conversation selects the whole scope: a ledger is
// read to answer "what is unreachable here", and the per-conversation read is
// the narrowing a host asks for when it is answering about one of them.
//
// The conversation narrows by the address the record was written with, so a
// document address -- which names no conversation -- is only in the whole-scope
// read. The read is one prefix scan and decodes every record: a record that
// cannot be read fails the call rather than being skipped, because a ledger a
// host reads as proof is the wrong place to hide a record.
func (store *Store) Forgotten(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
) ([]Record, error) {
	if store == nil || nilValue(store.kv) {
		return nil, errors.New("forget: store is incomplete")
	}
	if ctx == nil {
		return nil, errors.New("forget: context is required")
	}
	prefix, err := itemsPrefix(scope)
	if err != nil {
		return nil, err
	}
	entries, err := store.kv.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("forget: list ledger: %w", err)
	}
	conversation := strings.TrimSpace(conversationID)
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		record, err := decodeRecord(scope, prefix, entry.Key, entry.Value)
		if err != nil {
			return nil, err
		}
		if conversation != "" && record.Address.ConversationID != conversation {
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].less(records[j])
	})
	return records, nil
}

// Diagnostics reports one scope's ledger shape. The read is a key-only scan
// where the substrate offers one: what a host looks at in a health snapshot is
// how much is unreachable, not what the reasons say.
func (store *Store) Diagnostics(ctx context.Context, scope corememory.Scope) (Diagnostics, error) {
	if store == nil || nilValue(store.kv) {
		return Diagnostics{}, errors.New("forget: store is incomplete")
	}
	if ctx == nil {
		return Diagnostics{}, errors.New("forget: context is required")
	}
	prefix, err := itemsPrefix(scope)
	if err != nil {
		return Diagnostics{}, err
	}
	keys, err := keysUnder(ctx, store.kv, prefix)
	if err != nil {
		return Diagnostics{}, fmt.Errorf("forget: count ledger: %w", err)
	}
	return Diagnostics{Items: len(keys)}, nil
}

// Visible implements the retrieval provider's per-identity overlay read: it
// reports whether one identity may be served. The read is one point lookup, so
// an item the ledger does not cover costs one miss.
//
// A record that cannot be decoded is an error, not an absent record, and the
// provider treats a failing read as "do not serve this item". That is the
// direction a forget has to fail in: a ledger entry nobody can read is still a
// statement that the item was meant to be unreachable.
func (store *Store) Visible(
	ctx context.Context,
	scope corememory.Scope,
	identity string,
) (bool, error) {
	if store == nil || nilValue(store.kv) {
		return false, errors.New("forget: store is incomplete")
	}
	if ctx == nil {
		return false, errors.New("forget: context is required")
	}
	key, err := itemKey(scope, identity)
	if err != nil {
		return false, err
	}
	_, found, err := store.read(ctx, scope, key)
	if err != nil {
		return false, err
	}
	return !found, nil
}

// SoftForgotten implements the retrieval provider's bulk overlay read: every
// identity one scope's read path is told to drop, in one scan. The map is never
// nil, so a substrate that has nothing forgotten answers "nothing" rather than
// sending the provider back for per-item reads.
//
// The read is key-only where the substrate offers it, so it does not decode the
// reasons and times it does not need. A record it cannot decode is still
// honored: the identity is the key.
func (store *Store) SoftForgotten(ctx context.Context, scope corememory.Scope) (map[string]struct{}, error) {
	if store == nil || nilValue(store.kv) {
		return nil, errors.New("forget: store is incomplete")
	}
	if ctx == nil {
		return nil, errors.New("forget: context is required")
	}
	prefix, err := itemsPrefix(scope)
	if err != nil {
		return nil, err
	}
	keys, err := keysUnder(ctx, store.kv, prefix)
	if err != nil {
		return nil, fmt.Errorf("forget: list ledger: %w", err)
	}
	forgotten := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		identity, err := identityFromKey(prefix, key)
		if err != nil {
			return nil, err
		}
		forgotten[identity] = struct{}{}
	}
	return forgotten, nil
}

func (store *Store) read(ctx context.Context, scope corememory.Scope, key string) (Record, bool, error) {
	data, err := store.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Record{}, false, nil
		}
		return Record{}, false, fmt.Errorf("forget: load record: %w", err)
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, false, fmt.Errorf("forget: decode record: %w", err)
	}
	if err := validateRecord(scope, record); err != nil {
		return Record{}, false, fmt.Errorf("forget: corrupt record: %w", err)
	}
	if prefix, err := itemsPrefix(scope); err != nil {
		return Record{}, false, err
	} else if key != prefix+"/"+record.Identity {
		return Record{}, false, fmt.Errorf("forget: record %q is stored at %q", record.Identity, key)
	}
	return record, true, nil
}

func (store *Store) write(ctx context.Context, key string, record Record) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("forget: encode record %q: %w", record.Identity, err)
	}
	if err := store.kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("forget: save record %q: %w", record.Identity, err)
	}
	return nil
}

// less orders the ledger: oldest first, identity breaking a tie, so a listing
// is stable across reads and reads as the history it is.
func (record Record) less(other Record) bool {
	if !record.HiddenAt.Equal(other.HiddenAt) {
		return record.HiddenAt.Before(other.HiddenAt)
	}
	return record.Identity < other.Identity
}

// itemsPrefix names one scope's ledger area.
func itemsPrefix(scope corememory.Scope) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return rootPrefix + "/" + partition + "/items", nil
}

// planned is one address resolved into what a write needs: the identity the
// read path looks up and the key the record is stored at.
type planned struct {
	address  corememory.ContextAddress
	identity string
	key      string
}

// plan resolves every address of one call before the first write, and drops the
// addresses that name an identity already named in the same call. An address
// the ledger cannot address fails here, which is what keeps a call from
// recording half of what it was given.
func plan(scope corememory.Scope, addresses []corememory.ContextAddress) ([]planned, error) {
	entries := make([]planned, 0, len(addresses))
	seen := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		identity, err := identityFor(scope, address)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		key, err := itemKey(scope, identity)
		if err != nil {
			return nil, err
		}
		entries = append(entries, planned{address: address, identity: identity, key: key})
	}
	return entries, nil
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

// identityFor derives the identity one address is forgotten under. The address
// has to be one the read path can serve: an address that names nothing (a zero
// address) or a shape without coordinates would record a forget no read ever
// looks up.
func identityFor(scope corememory.Scope, address corememory.ContextAddress) (string, error) {
	if err := validateAddress(address); err != nil {
		return "", err
	}
	if err := scope.Validate(); err != nil {
		return "", fmt.Errorf("forget: %w", err)
	}
	return address.Identity(scope), nil
}

// validateAddress rejects an address no read path would look up: a zero one, a
// shape without its coordinates, and the recent lane, which is never filtered.
// A raw message is the conversation's own last turns, which an answer is built
// on, so a ledger record for one would be a forget that does nothing -- refused
// instead of recorded.
func validateAddress(address corememory.ContextAddress) error {
	if address.IsZero() {
		return errors.New("forget: every entry needs an address")
	}
	if err := address.Validate(); err != nil {
		return fmt.Errorf("forget: %w", err)
	}
	if address.Kind == corememory.ContextRawMessage {
		return errors.New("forget: the recent lane is never hidden, so a raw message cannot be forgotten")
	}
	return nil
}

// identityFromKey reads the identity back out of one key below a ledger
// prefix: the segment the store wrote with. A key with no segment, or one that
// could not be an identity, is not a key this store wrote.
func identityFromKey(prefix, key string) (string, error) {
	identity, ok := strings.CutPrefix(key, prefix+"/")
	if !ok {
		return "", fmt.Errorf("forget: key %q is outside the ledger", key)
	}
	if err := validateIdentity(identity); err != nil {
		return "", err
	}
	return identity, nil
}

// keysUnder reads the keys below one ledger area. The scan is key-only when
// the substrate offers it: a request needs the identities alone, and the
// records carry reasons and times it would only decode to discard.
func keysUnder(ctx context.Context, kv storage.Store, prefix string) ([]string, error) {
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

// validateIdentity keeps one record one path segment. An identity that carried
// a separator could collide with another record's address, and the repository's
// identities are opaque single segments by construction.
func validateIdentity(identity string) error {
	switch {
	case strings.TrimSpace(identity) == "":
		return errors.New("forget: item identity is required")
	case identity != strings.TrimSpace(identity):
		return errors.New("forget: item identity must be trimmed")
	case strings.Contains(identity, "/"):
		return errors.New("forget: item identity must be one path segment")
	case strings.ContainsRune(identity, '\x00'):
		return errors.New("forget: item identity must not contain NUL")
	}
	return nil
}

func validateReason(reason string) error {
	if reason != strings.TrimSpace(reason) {
		return errors.New("forget: reason must be trimmed")
	}
	if len(reason) > maxReasonBytes {
		return fmt.Errorf("forget: reason is %d bytes, the limit is %d", len(reason), maxReasonBytes)
	}
	if strings.ContainsRune(reason, '\x00') {
		return errors.New("forget: reason must not contain NUL")
	}
	return nil
}

func validateRecord(scope corememory.Scope, record Record) error {
	if err := validateIdentity(record.Identity); err != nil {
		return err
	}
	if err := validateReason(record.Reason); err != nil {
		return err
	}
	if record.HiddenAt.IsZero() {
		return errors.New("forget: record has no hide time")
	}
	if err := validateAddress(record.Address); err != nil {
		return err
	}
	if record.Address.Identity(scope) != record.Identity {
		return errors.New("forget: record identity does not match its address")
	}
	return nil
}

func decodeRecord(scope corememory.Scope, prefix, key string, value []byte) (Record, error) {
	identity, err := identityFromKey(prefix, key)
	if err != nil {
		return Record{}, err
	}
	var record Record
	if err := json.Unmarshal(value, &record); err != nil {
		return Record{}, fmt.Errorf("forget: decode record %q: %w", identity, err)
	}
	if err := validateRecord(scope, record); err != nil {
		return Record{}, fmt.Errorf("forget: corrupt record %q: %w", identity, err)
	}
	if record.Identity != identity {
		return Record{}, fmt.Errorf("forget: record %q names identity %q", identity, record.Identity)
	}
	return record, nil
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
