package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

const (
	// rootPrefix is the block store's namespace. One document per
	// conversation, so a request reads the block of the conversation it is
	// serving and a pass rewrites one conversation's block at a time.
	rootPrefix = "profile/v1/blocks"
	// maxWriteAttempts bounds the read-modify-write retries a write makes when
	// the other writer lands in between. Two writers share one document -- the
	// host's half and the pass's half -- so a write swaps against what it
	// read rather than overwriting what it did not.
	maxWriteAttempts = 5
)

// Store persists one block per conversation on the KV substrate.
type Store struct {
	kv       storage.Store
	cas      storage.CASStore
	putOnce  storage.PutIfAbsentStore
	clock    func() time.Time
	config   Config
	producer string
}

// Option configures the block store.
type Option func(*Store)

// WithClock replaces the store's clock, which stamps a write's UpdatedAt.
func WithClock(clock func() time.Time) Option {
	return func(store *Store) {
		if clock != nil {
			store.clock = clock
		}
	}
}

// WithConfig sets the store's bound and distillation policy. The zero config is
// the default bound with distillation off.
func WithConfig(config Config) Option {
	return func(store *Store) { store.config = config }
}

// NewStore builds a KV-backed block store.
func NewStore(kv storage.Store, options ...Option) (*Store, error) {
	if nilValue(kv) {
		return nil, errors.New("profile: kv store is required")
	}
	swapper, ok := kv.(storage.CASStore)
	if !ok {
		return nil, errors.New("profile: kv store does not support compare-and-swap, which the block's two writers need")
	}
	putOnce, ok := kv.(storage.PutIfAbsentStore)
	if !ok {
		return nil, errors.New("profile: kv store does not support put-if-absent, which creating a block needs")
	}
	store := &Store{kv: kv, cas: swapper, putOnce: putOnce, clock: time.Now}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	store.config = store.config.withDefaults()
	if err := store.config.validate(); err != nil {
		return nil, err
	}
	store.producer = store.config.ProducerVersion()
	return store, nil
}

// Config returns the bound and policy the store serves.
func (store *Store) Config() Config { return store.config }

// ProducerVersion returns the policy stamp distilled entries are served under.
func (store *Store) ProducerVersion() string { return store.producer }

// Load returns one conversation's stored block, whatever policy wrote it.
// Serving goes through Block, which applies the policy gate and the bound; Load
// is what a host reads to edit a block without losing the other half.
func (store *Store) Load(ctx context.Context, scope corememory.Scope, conversationID string) (Document, bool, error) {
	if err := store.ready(ctx); err != nil {
		return Document{}, false, err
	}
	document, _, found, err := store.read(ctx, scope, conversationID)
	if err != nil {
		return Document{}, false, err
	}
	return document, found, nil
}

// Block returns the block the read path serves for one conversation: the host's
// half, then the pass's half when this policy judged it. A conversation with no
// block, or one whose stored halves amount to nothing servable, reports no
// block rather than an empty one.
func (store *Store) Block(ctx context.Context, scope corememory.Scope, conversationID string) (Block, bool, error) {
	block, _, err := store.BlockServing(ctx, scope, conversationID, nil)
	if err != nil {
		return Block{}, false, err
	}
	if block.Entries == 0 {
		return Block{}, false, nil
	}
	return block, true, nil
}

// BlockServing is Block for a read path that hides items by identity: a
// distilled line whose provenance names a fact the filter hides is dropped
// from the served block, so a hide reaches the lines a pass drew from the
// facts it covers. The count is how many lines went.
//
// A line dropped this way is not the same reading as Truncated: the count is
// what the read path decided not to serve, and Truncated is what the bound
// could not hold. Explicit lines are the host's own words and are never
// offered to the filter, the same way a hide covers what the views derived
// from a turn and not the turn itself; a host that wants one gone edits the
// block.
//
// A conversation with no servable block returns the zero Block, a zero count
// and no error: the block is an addition to a request, and a request without
// one is served the way a deployment without the block is.
func (store *Store) BlockServing(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
	hide func(factID string) (bool, error),
) (Block, int, error) {
	if err := store.ready(ctx); err != nil {
		return Block{}, 0, err
	}
	document, _, found, err := store.read(ctx, scope, conversationID)
	if err != nil {
		return Block{}, 0, err
	}
	if !found {
		return Block{}, 0, nil
	}
	served := store.serve(document)
	hidden := 0
	if hide != nil && len(served) > 0 {
		kept := make([]Entry, 0, len(served))
		for _, entry := range served {
			hides, err := hidesEntry(entry, hide)
			if err != nil {
				return Block{}, 0, err
			}
			if hides {
				hidden++
				continue
			}
			kept = append(kept, entry)
		}
		served = kept
	}
	truncated := false
	for len(served) > 0 && !store.fits(served) {
		// A block stored under a larger cap than this store serves: the tail
		// is dropped and reported rather than served over the bound. Only a
		// configuration change can get here -- every write is validated.
		served = served[:len(served)-1]
		truncated = true
	}
	if len(served) == 0 {
		return Block{}, hidden, nil
	}
	sources := make([]corememory.SourceRef, 0, len(served))
	for _, entry := range served {
		sources = append(sources, entrySources(conversationID, entry))
	}
	return Block{
		Text:      Render(served),
		Entries:   len(served),
		Sources:   sources,
		Truncated: truncated,
	}, hidden, nil
}

// hidesEntry asks a read path's filter about one entry: only a distilled line
// with a provenance is asked at all, because what the filter decides about is
// facts, and a line without one was not drawn from the corpus. One hidden fact
// hides the line: the text is what the pass judged from those facts, and
// serving it would serve what the hide covered.
func hidesEntry(entry Entry, hide func(factID string) (bool, error)) (bool, error) {
	if entry.Origin != OriginDistilled || len(entry.Provenance) == 0 {
		return false, nil
	}
	for _, factID := range entry.Provenance {
		hidden, err := hide(factID)
		if err != nil {
			return true, err
		}
		if hidden {
			return true, nil
		}
	}
	return false, nil
}

// serve is the read path's half of the policy gate: the host's lines always,
// the pass's lines only when the policy this store serves was the one that
// judged them.
func (store *Store) serve(document Document) []Entry {
	served := append([]Entry(nil), document.Explicit...)
	if store.config.Distill && document.Producer == store.producer {
		served = append(served, document.Distilled...)
	}
	return served
}

// SaveExplicit replaces the host-written half of one conversation's block. The
// texts are normalized, deduplicated by content and validated before anything
// is written: a call that names an entry the block cannot hold writes nothing.
//
// The host's half is authoritative, so a write that would exceed the bound
// makes room by dropping the pass's lowest-ranked lines -- the pass re-judges
// them on its next run -- and is only refused when the host's lines alone do
// not fit.
func (store *Store) SaveExplicit(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
	texts []string,
) (SaveResult, error) {
	if err := store.ready(ctx); err != nil {
		return SaveResult{}, err
	}
	at := store.clock().UTC()
	if at.IsZero() {
		return SaveResult{}, errors.New("profile: clock returned zero time")
	}
	entries, err := explicitEntries(texts, at)
	if err != nil {
		return SaveResult{}, err
	}
	if !store.fits(entries) {
		return SaveResult{}, fmt.Errorf(
			"profile: %d explicit entries do not fit the block (%d tokens, %d entries maximum)",
			len(entries), store.config.MaxTokens, store.config.MaxEntries)
	}
	return store.mutate(ctx, scope, conversationID, func(current Document) (blockChange, error) {
		next := current
		next.Explicit = entries
		next.UpdatedAt = at
		dropped := 0
		for len(next.Distilled) > 0 && !store.fits(append(append([]Entry(nil), next.Explicit...), next.Distilled...)) {
			next.Distilled = next.Distilled[:len(next.Distilled)-1]
			dropped++
		}
		return blockChange{document: next, dropped: dropped}, nil
	})
}

// SaveDistilled replaces the pass-written half of one conversation's block and
// stamps it with this store's policy. It is the pass's write, so the pass is
// responsible for ranking and truncating; the store validates the shape and
// refuses a half that does not fit on its own, which keeps every stored block
// within the bound by construction.
func (store *Store) SaveDistilled(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
	entries []Entry,
	at time.Time,
) (SaveResult, error) {
	if err := store.ready(ctx); err != nil {
		return SaveResult{}, err
	}
	if at.IsZero() {
		return SaveResult{}, errors.New("profile: write time is required")
	}
	return store.saveDistilled(ctx, scope, conversationID, entries, at)
}

// SaveDistilledConversations replaces the pass-written half of the named
// conversations in one call. A named conversation with no entries has its half
// cleared, which is how a pass that found nothing drops what an earlier pass
// wrote there.
func (store *Store) SaveDistilledConversations(
	ctx context.Context,
	scope corememory.Scope,
	blocks map[string][]Entry,
	at time.Time,
) (int, error) {
	if err := store.ready(ctx); err != nil {
		return 0, err
	}
	if len(blocks) == 0 {
		return 0, errors.New("profile: at least one conversation is required")
	}
	if at.IsZero() {
		return 0, errors.New("profile: write time is required")
	}
	// Every conversation is validated before the first write, so a call that
	// names one block the store cannot hold writes none of them.
	prepared := make(map[string][]Entry, len(blocks))
	for conversationID, entries := range blocks {
		stamped, err := store.distilledEntries(entries, at)
		if err != nil {
			return 0, fmt.Errorf("profile: conversation %q: %w", conversationID, err)
		}
		if !store.fits(stamped) {
			return 0, fmt.Errorf(
				"profile: conversation %q: %d distilled entries do not fit the block (%d tokens, %d entries maximum)",
				conversationID, len(stamped), store.config.MaxTokens, store.config.MaxEntries)
		}
		prepared[conversationID] = stamped
	}
	written := 0
	for _, conversationID := range slices.Sorted(maps.Keys(prepared)) {
		if _, err := store.writeDistilled(ctx, scope, conversationID, prepared[conversationID], at); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// saveDistilled validates one conversation's half and writes it.
func (store *Store) saveDistilled(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
	entries []Entry,
	at time.Time,
) (SaveResult, error) {
	stamped, err := store.distilledEntries(entries, at)
	if err != nil {
		return SaveResult{}, err
	}
	if !store.fits(stamped) {
		return SaveResult{}, fmt.Errorf(
			"profile: %d distilled entries do not fit the block (%d tokens, %d entries maximum)",
			len(stamped), store.config.MaxTokens, store.config.MaxEntries)
	}
	return store.writeDistilled(ctx, scope, conversationID, stamped, at)
}

// writeDistilled swaps one conversation's distilled half for entries already
// validated and stamped.
func (store *Store) writeDistilled(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
	entries []Entry,
	at time.Time,
) (SaveResult, error) {
	return store.mutate(ctx, scope, conversationID, func(current Document) (blockChange, error) {
		next := current
		next.Distilled = entries
		next.Producer = store.producer
		next.UpdatedAt = at
		dropped := 0
		for len(next.Distilled) > 0 && !store.fits(append(append([]Entry(nil), next.Explicit...), next.Distilled...)) {
			next.Distilled = next.Distilled[:len(next.Distilled)-1]
			dropped++
		}
		return blockChange{document: next, dropped: dropped}, nil
	})
}

// Clear removes one conversation's block, both halves.
func (store *Store) Clear(ctx context.Context, scope corememory.Scope, conversationID string) error {
	if err := store.ready(ctx); err != nil {
		return err
	}
	key, err := store.key(scope, conversationID)
	if err != nil {
		return err
	}
	if err := store.kv.Delete(ctx, key); err != nil {
		return fmt.Errorf("profile: clear block: %w", err)
	}
	return nil
}

// Conversations lists the conversations holding a block in one scope.
func (store *Store) Conversations(ctx context.Context, scope corememory.Scope) ([]string, error) {
	if err := store.ready(ctx); err != nil {
		return nil, err
	}
	prefix, err := store.scopePrefix(scope)
	if err != nil {
		return nil, err
	}
	var keys []string
	if lister, ok := store.kv.(storage.KeyLister); ok {
		keys, err = lister.Keys(ctx, prefix)
		if err != nil {
			return nil, fmt.Errorf("profile: list blocks: %w", err)
		}
	} else {
		entries, listErr := store.kv.List(ctx, prefix)
		if listErr != nil {
			return nil, fmt.Errorf("profile: list blocks: %w", listErr)
		}
		for _, entry := range entries {
			keys = append(keys, entry.Key)
		}
	}
	conversations := make([]string, 0, len(keys))
	for _, key := range keys {
		segment := key[strings.LastIndex(key, "/")+1:]
		conversationID, err := storage.DecodeSegment(segment)
		if err != nil {
			return nil, fmt.Errorf("profile: decode block key %q: %w", key, err)
		}
		conversations = append(conversations, conversationID)
	}
	sort.Strings(conversations)
	return conversations, nil
}

// Diagnostics reports one scope's blocks without serving them: what the scope
// holds and what the bound allows.
type Diagnostics struct {
	Blocks     int    `json:"blocks"`
	Entries    int    `json:"entries"`
	Explicit   int    `json:"explicit"`
	Distilled  int    `json:"distilled"`
	Tokens     int    `json:"tokens"`
	MaxTokens  int    `json:"max_tokens"`
	MaxEntries int    `json:"max_entries"`
	Producer   string `json:"producer"`
	Distill    bool   `json:"distill"`
}

// Diagnostics reads every block of one scope. It is a reading, not a control:
// the scope's blocks are bounded and few (one per conversation), so a host can
// report what the read path would serve.
func (store *Store) Diagnostics(ctx context.Context, scope corememory.Scope) (Diagnostics, error) {
	if err := store.ready(ctx); err != nil {
		return Diagnostics{}, err
	}
	prefix, err := store.scopePrefix(scope)
	if err != nil {
		return Diagnostics{}, err
	}
	entries, err := store.kv.List(ctx, prefix)
	if err != nil {
		return Diagnostics{}, fmt.Errorf("profile: list blocks: %w", err)
	}
	report := Diagnostics{
		MaxTokens: store.config.MaxTokens, MaxEntries: store.config.MaxEntries,
		Producer: store.producer, Distill: store.config.Distill,
	}
	for _, entry := range entries {
		var document Document
		if err := decodeJSON(entry.Value, &document); err != nil {
			return Diagnostics{}, fmt.Errorf("profile: decode block %q: %w", entry.Key, err)
		}
		if err := document.validate(); err != nil {
			return Diagnostics{}, fmt.Errorf("profile: block %q: %w", entry.Key, err)
		}
		served := store.serve(document)
		report.Blocks++
		report.Entries += len(served)
		report.Explicit += len(document.Explicit)
		report.Distilled += len(document.Distilled)
		report.Tokens += entriesTokens(served)
	}
	return report, nil
}

// document is the mutable shape a write produces. dropped counts the distilled
// entries the change removed to keep the block within its bound.
type blockChange struct {
	document Document
	dropped  int
}

func (store *Store) mutate(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
	change func(Document) (blockChange, error),
) (SaveResult, error) {
	key, err := store.key(scope, conversationID)
	if err != nil {
		return SaveResult{}, err
	}
	for attempt := 0; attempt < maxWriteAttempts; attempt++ {
		current, raw, found, err := store.read(ctx, scope, conversationID)
		if err != nil {
			return SaveResult{}, err
		}
		if !found {
			current = Document{}
		}
		change, err := change(current)
		if err != nil {
			return SaveResult{}, err
		}
		next := change.document
		next.SchemaVersion = schemaVersion
		if err := next.validate(); err != nil {
			return SaveResult{}, err
		}
		if len(next.Explicit) == 0 && len(next.Distilled) == 0 {
			// A block with nothing in it is no block: the key goes away, so a
			// cleared conversation stops being listed as one that has one.
			if !found {
				return SaveResult{Document: next}, nil
			}
			if err := store.kv.Delete(ctx, key); err != nil {
				return SaveResult{}, fmt.Errorf("profile: clear empty block: %w", err)
			}
			return SaveResult{Document: next, Dropped: change.dropped}, nil
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			return SaveResult{}, fmt.Errorf("profile: encode block: %w", err)
		}
		if !found {
			created, err := store.putOnce.PutIfAbsent(ctx, key, encoded)
			if err != nil {
				return SaveResult{}, fmt.Errorf("profile: create block: %w", err)
			}
			if created {
				return SaveResult{Document: next, Dropped: change.dropped}, nil
			}
			continue
		}
		swapped, err := store.cas.CompareAndSwap(ctx, key, raw, encoded)
		if err != nil {
			return SaveResult{}, fmt.Errorf("profile: write block: %w", err)
		}
		if swapped {
			return SaveResult{Document: next, Dropped: change.dropped}, nil
		}
	}
	return SaveResult{}, fmt.Errorf("profile: block %q changed under %d consecutive writes", conversationID, maxWriteAttempts)
}

// read returns the stored document together with the bytes it was decoded from,
// which is what a rewrite swaps against.
func (store *Store) read(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
) (Document, []byte, bool, error) {
	key, err := store.key(scope, conversationID)
	if err != nil {
		return Document{}, nil, false, err
	}
	raw, err := store.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Document{}, nil, false, nil
		}
		return Document{}, nil, false, fmt.Errorf("profile: read block: %w", err)
	}
	var document Document
	if err := decodeJSON(raw, &document); err != nil {
		return Document{}, nil, false, fmt.Errorf("profile: decode block: %w", err)
	}
	if err := document.validate(); err != nil {
		return Document{}, nil, false, err
	}
	return document, raw, true, nil
}

func (store *Store) ready(ctx context.Context) error {
	if store == nil || nilValue(store.kv) {
		return errors.New("profile: store is incomplete")
	}
	if ctx == nil {
		return errors.New("profile: context is required")
	}
	return nil
}

func (store *Store) key(scope corememory.Scope, conversationID string) (string, error) {
	prefix, err := store.scopePrefix(scope)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(conversationID) == "" {
		return "", errors.New("profile: conversation id is required")
	}
	if strings.ContainsRune(conversationID, '\x00') {
		return "", errors.New("profile: conversation id must not contain NUL")
	}
	return prefix + "/" + storage.EncodeSegment(conversationID), nil
}

func (store *Store) scopePrefix(scope corememory.Scope) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", fmt.Errorf("profile: scope partition: %w", err)
	}
	return rootPrefix + "/" + partition, nil
}

// fits reports whether the entries stay within the block's bound on their own.
func (store *Store) fits(entries []Entry) bool {
	return len(entries) <= store.config.MaxEntries && entriesTokens(entries) <= store.config.MaxTokens
}

// explicitEntries normalizes and validates the host's lines: folded text,
// content-derived ids, no duplicates, first appearance order kept.
func explicitEntries(texts []string, at time.Time) ([]Entry, error) {
	entries := make([]Entry, 0, len(texts))
	seen := make(map[string]struct{}, len(texts))
	for index, text := range texts {
		normalized := foldText(text)
		if normalized == "" {
			return nil, fmt.Errorf("profile: entry %d is empty", index)
		}
		id := EntryID(normalized)
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("profile: entry %q is repeated", normalized)
		}
		seen[id] = struct{}{}
		entries = append(entries, Entry{ID: id, Text: normalized, Origin: OriginExplicit, UpdatedAt: at})
	}
	return entries, nil
}

// distilledEntries validates the pass's half. The pass supplies the ranking;
// the store stamps the origin, the ids and the provenance.
func (store *Store) distilledEntries(entries []Entry, at time.Time) ([]Entry, error) {
	stamped := make([]Entry, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for index, entry := range entries {
		text := foldText(entry.Text)
		if text == "" {
			return nil, fmt.Errorf("profile: distilled entry %d is empty", index)
		}
		id := EntryID(text)
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("profile: distilled entry %q is repeated", text)
		}
		seen[id] = struct{}{}
		stamped = append(stamped, Entry{
			ID: id, Text: text, Origin: OriginDistilled,
			Provenance: slices.Sorted(slices.Values(entry.Provenance)), UpdatedAt: at,
		})
	}
	return stamped, nil
}

// validate rejects a document the read path must not serve: a shape this
// version does not know, or an entry whose id and text disagree.
func (document Document) validate() error {
	if document.SchemaVersion != schemaVersion {
		return fmt.Errorf("profile: unsupported block schema_version %d", document.SchemaVersion)
	}
	for index, entry := range document.Explicit {
		if entry.Origin != OriginExplicit {
			return fmt.Errorf("profile: explicit entry %d has origin %q", index, entry.Origin)
		}
		if err := validateEntry(entry); err != nil {
			return err
		}
	}
	for index, entry := range document.Distilled {
		if entry.Origin != OriginDistilled {
			return fmt.Errorf("profile: distilled entry %d has origin %q", index, entry.Origin)
		}
		if err := validateEntry(entry); err != nil {
			return err
		}
	}
	return nil
}

func decodeJSON(data []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
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
