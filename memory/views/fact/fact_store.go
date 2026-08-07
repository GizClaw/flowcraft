package fact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/memory/storage"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
)

const schemaVersion = 2

const mergeEventType = "fact.merge"

// Option configures a FactStore.
type Option func(*FactStore)

// WithClock replaces the clock used for authoritative timestamps.
func WithClock(clock func() time.Time) Option {
	return func(store *FactStore) {
		if clock != nil {
			store.clock = clock
		}
	}
}

// WithPublicationSink attaches a durable outbox publisher. The sink is called
// only after the immutable fact/merge record is durable.
func WithPublicationSink(sink PublicationSink) Option {
	return func(store *FactStore) { store.publications = sink }
}

// FactStore stores each immutable base fact as a KV snapshot and every merge
// event as a storage.Log append. Reads aggregate the base with replayed merge
// events, so the KV snapshot is a cache that repair can rebuild from the Log.
type FactStore struct {
	log          storage.Log
	kv           storage.Store
	clock        func() time.Time
	publications PublicationSink
	mu           sync.RWMutex
}

type persistedFact struct {
	SchemaVersion  int    `json:"schema_version"`
	RuntimeID      string `json:"runtime_id"`
	UserID         string `json:"user_id"`
	AgentID        string `json:"agent_id,omitempty"`
	ConversationID string `json:"conversation_id"`
	FactID         string `json:"fact_id"`
	Fact           Fact   `json:"fact"`
}

type mergeEvent struct {
	SchemaVersion   int                   `json:"schema_version"`
	FactID          string                `json:"fact_id"`
	CanonicalHash   string                `json:"canonical_hash"`
	Provenance      []sdkmemory.SourceRef `json:"provenance"`
	LinkedMemoryIDs []string              `json:"linked_memory_ids,omitempty"`
	Entities        []string              `json:"entities,omitempty"`
	SourceDigest    string                `json:"source_digest"`
	EventTime       time.Time             `json:"event_time"`
}

// NewFactStore constructs a Log+KV backed fact view. The KV backend must
// support immutable writes for base fact snapshots.
func NewFactStore(log storage.Log, kv storage.Store, options ...Option) (*FactStore, error) {
	if nilValue(log) || nilValue(kv) {
		return nil, errors.New("fact view: log and store are required")
	}
	if _, ok := kv.(storage.PutIfAbsentStore); !ok {
		return nil, errors.New("fact view: store must support immutable writes")
	}
	store := &FactStore{log: log, kv: kv, clock: time.Now}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	return store, nil
}

// Add appends one immutable fact or merges it into an existing canonical
// identity.
func (store *FactStore) Add(ctx context.Context, request AddRequest) (Fact, error) {
	if err := validateAdd(request); err != nil {
		return Fact{}, err
	}
	now := store.clock()
	if now.IsZero() {
		return Fact{}, errors.New("fact view: clock returned zero time")
	}
	text := NormalizeText(request.Content.Text())
	canonicalHash := request.CanonicalHash
	if canonicalHash == "" {
		canonicalHash = CanonicalHash(text)
	}
	if canonicalHash != CanonicalHash(text) {
		return Fact{}, errors.New("fact view: canonical_hash does not match content")
	}
	eventTime := request.EventTime
	if eventTime.IsZero() {
		eventTime = now
	}
	sourceDigest := strings.TrimSpace(request.SourceDigest)
	if sourceDigest == "" {
		sourceDigest = digestSources(request.Provenance)
	}
	transformSignature := strings.TrimSpace(request.TransformSignature)
	if transformSignature == "" {
		transformSignature = "manual-v1"
	}
	candidate := Fact{
		ID: request.ID, CanonicalHash: canonicalHash,
		Scope: request.Scope, ConversationID: request.ConversationID,
		Text: text, Content: factTextContent(text), Entities: NormalizeEntities(request.Entities),
		Predicate: NormalizeText(request.Predicate), TemporalDetail: NormalizeText(request.TemporalDetail),
		EventTime: eventTime, LinkedMemoryIDs: normalizeIDs(request.LinkedMemoryIDs, request.ID),
		Provenance:   append([]sdkmemory.SourceRef(nil), request.Provenance...),
		SourceDigest: sourceDigest, TransformSignature: transformSignature,
		Metadata: request.Metadata.Clone(), CreatedAt: now,
	}
	sortSources(candidate.Provenance)
	if err := validateFact(candidate); err != nil {
		return Fact{}, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	existing, ok, err := store.findByCanonicalHash(ctx, request.Scope, request.ConversationID, canonicalHash)
	if err != nil {
		return Fact{}, err
	}
	if ok {
		if CanonicalContent(existing.Fact.Text) != CanonicalContent(candidate.Text) {
			return Fact{}, errdefs.Conflictf("fact view: canonical hash collision %q", canonicalHash)
		}
		current, err := store.aggregate(ctx, existing)
		if err != nil {
			return Fact{}, err
		}
		if stateContains(existing.Fact, candidate) {
			if err := store.publish(ctx, current, "published", factRevisionDigest(existing.Fact)); err != nil {
				return Fact{}, err
			}
			return current, nil
		}
		event := mergeEvent{
			SchemaVersion: schemaVersion, FactID: existing.Fact.ID, CanonicalHash: canonicalHash,
			Provenance: candidate.Provenance, LinkedMemoryIDs: normalizeIDs(candidate.LinkedMemoryIDs, existing.Fact.ID),
			Entities: candidate.Entities, SourceDigest: candidate.SourceDigest, EventTime: candidate.EventTime,
		}
		revisionDigest, err := store.writeMergeEvent(ctx, request.Scope, request.ConversationID, event)
		if err != nil {
			return Fact{}, err
		}
		merged, err := store.aggregate(ctx, existing)
		if err != nil {
			return Fact{}, err
		}
		if err := store.publish(ctx, merged, "merged", revisionDigest); err != nil {
			return Fact{}, err
		}
		return merged, nil
	}
	if byID, exists, err := store.read(ctx, request.Scope, request.ConversationID, request.ID); err != nil {
		return Fact{}, err
	} else if exists {
		return Fact{}, errdefs.Conflictf("fact view: fact %q already exists with canonical hash %q", request.ID, byID.Fact.CanonicalHash)
	}
	persisted := persistedFact{
		SchemaVersion: schemaVersion, RuntimeID: request.Scope.RuntimeID,
		UserID: request.Scope.UserID, AgentID: request.Scope.AgentID, ConversationID: request.ConversationID,
		FactID: request.ID, Fact: candidate,
	}
	data, err := json.Marshal(persisted)
	if err != nil {
		return Fact{}, fmt.Errorf("fact view: encode fact %q: %w", request.ID, err)
	}
	key, err := store.factKey(request.Scope, request.ConversationID, request.ID)
	if err != nil {
		return Fact{}, err
	}
	if err := store.putImmutable(ctx, key, data); err != nil {
		return Fact{}, fmt.Errorf("fact view: write fact %q: %w", request.ID, err)
	}
	if err := store.publish(ctx, candidate, "published", factRevisionDigest(candidate)); err != nil {
		return Fact{}, err
	}
	return cloneFact(candidate), nil
}

func (store *FactStore) publish(ctx context.Context, value Fact, event, revisionDigest string) error {
	if store.publications == nil {
		return nil
	}
	publication := Publication{
		Fact: cloneFact(value), Event: event, RevisionDigest: revisionDigest,
		PublicationID: publicationID(value.Scope, value.ConversationID, value.ID, event, revisionDigest),
	}
	if err := store.publications.PublishFact(ctx, publication); err != nil {
		return fmt.Errorf("fact view: publish lifecycle event: %w", err)
	}
	return nil
}

// Get returns the aggregated fact for one ID.
func (store *FactStore) Get(ctx context.Context, scope sdkmemory.Scope, conversationID, factID string) (Fact, bool, error) {
	if err := validateAddress(scope, conversationID); err != nil {
		return Fact{}, false, err
	}
	if strings.TrimSpace(factID) == "" {
		return Fact{}, false, errors.New("fact view: fact_id is required")
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	persisted, ok, err := store.read(ctx, scope, conversationID, factID)
	if err != nil || !ok {
		return Fact{}, ok, err
	}
	aggregated, err := store.aggregate(ctx, persisted)
	return aggregated, err == nil, err
}

// List returns aggregated facts in one conversation, ordered by (CreatedAt,
// ID).
func (store *FactStore) List(ctx context.Context, scope sdkmemory.Scope, conversationID string, options ListOptions) ([]Fact, error) {
	if err := validateAddress(scope, conversationID); err != nil {
		return nil, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.listLocked(ctx, scope, conversationID, options)
}

// ListScope scans aggregated facts across every conversation in one hard
// scope.
func (store *FactStore) ListScope(ctx context.Context, scope sdkmemory.Scope) ([]Fact, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	result, err := store.listScopeLocked(ctx, scope)
	if err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ConversationID != result[j].ConversationID {
			return result[i].ConversationID < result[j].ConversationID
		}
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}

// ListPublications reconstructs every immutable initial and merge publication
// so outbox reconciliation preserves per-revision task identity.
func (store *FactStore) ListPublications(ctx context.Context, scope sdkmemory.Scope) ([]Publication, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	facts, err := store.listScopeLocked(ctx, scope)
	if err != nil {
		return nil, err
	}
	var result []Publication
	for _, current := range facts {
		base, ok, err := store.read(ctx, scope, current.ConversationID, current.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("fact view: fact %q disappeared during publication scan", current.ID)
		}
		revision := factRevisionDigest(base.Fact)
		result = append(result, Publication{
			Fact: current, Event: "published", RevisionDigest: revision,
			PublicationID: publicationID(scope, current.ConversationID, current.ID, "published", revision),
		})
		events, err := store.mergeEvents(ctx, scope, current.ConversationID, current.ID)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			eventID, err := mergeEventID(event.Payload)
			if err != nil {
				return nil, err
			}
			result = append(result, Publication{
				Fact: current, Event: "merged", RevisionDigest: eventID,
				PublicationID: publicationID(scope, current.ConversationID, current.ID, "merged", eventID),
			})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].PublicationID < result[j].PublicationID })
	return result, nil
}

// AuditSourceDigests reads each immutable fact and merge authority record and
// independently rehashes only that record's provenance.
func (store *FactStore) AuditSourceDigests(ctx context.Context, scope sdkmemory.Scope, conversationID string) ([]SourceDigestEvidence, error) {
	if err := validateAddress(scope, conversationID); err != nil {
		return nil, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	values, err := store.listLocked(ctx, scope, conversationID, ListOptions{})
	if err != nil {
		return nil, err
	}
	var result []SourceDigestEvidence
	for _, value := range values {
		base, ok, err := store.read(ctx, scope, conversationID, value.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("fact view: fact %q disappeared during source audit", value.ID)
		}
		result = append(result, SourceDigestEvidence{
			Name: "fact:" + value.ID, StoredDigest: base.Fact.SourceDigest,
			ComputedDigest: digestSources(base.Fact.Provenance),
		})
		events, err := store.mergeEvents(ctx, scope, conversationID, value.ID)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			var merge mergeEvent
			if err := decodeJSON(event.Payload, &merge); err != nil {
				return nil, err
			}
			eventID, err := mergeEventID(event.Payload)
			if err != nil {
				return nil, err
			}
			result = append(result, SourceDigestEvidence{
				Name: "merge:" + value.ID + ":" + eventID, StoredDigest: merge.SourceDigest,
				ComputedDigest: digestSources(merge.Provenance),
			})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (store *FactStore) listScopeLocked(ctx context.Context, scope sdkmemory.Scope) ([]Fact, error) {
	prefix, err := store.scopePrefix(scope)
	if err != nil {
		return nil, err
	}
	entries, err := store.kv.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var result []Fact
	for _, entry := range entries {
		conversationID, factID, err := decodeScopeFactKey(prefix, entry.Key)
		if err != nil {
			return nil, fmt.Errorf("fact view: decode fact key %q: %w", entry.Key, err)
		}
		persisted, ok, err := store.read(ctx, scope, conversationID, factID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("fact view: fact %q disappeared during scan", factID)
		}
		aggregated, err := store.aggregate(ctx, persisted)
		if err != nil {
			return nil, err
		}
		result = append(result, aggregated)
	}
	return result, nil
}

func (store *FactStore) listLocked(ctx context.Context, scope sdkmemory.Scope, conversationID string, options ListOptions) ([]Fact, error) {
	prefix, err := store.conversationPrefix(scope, conversationID)
	if err != nil {
		return nil, err
	}
	entries, err := store.kv.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	facts := make([]Fact, 0, len(entries))
	for _, entry := range entries {
		_, factID, err := decodeFactKey(prefix, entry.Key)
		if err != nil {
			return nil, fmt.Errorf("fact view: decode fact filename %q: %w", entry.Key, err)
		}
		persisted, ok, err := store.read(ctx, scope, conversationID, factID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("fact view: fact %q disappeared during scan", factID)
		}
		aggregated, err := store.aggregate(ctx, persisted)
		if err != nil {
			return nil, err
		}
		facts = append(facts, aggregated)
	}
	sort.Slice(facts, func(i, j int) bool {
		if facts[i].CreatedAt.Equal(facts[j].CreatedAt) {
			return facts[i].ID < facts[j].ID
		}
		return facts[i].CreatedAt.Before(facts[j].CreatedAt)
	})
	result := make([]Fact, 0)
	for _, item := range facts {
		if item.CreatedAt.Before(options.AfterCreatedAt) ||
			(item.CreatedAt.Equal(options.AfterCreatedAt) && item.ID <= options.AfterID) {
			continue
		}
		result = append(result, cloneFact(item))
		if options.Limit > 0 && len(result) == options.Limit {
			break
		}
	}
	return result, nil
}

func (store *FactStore) findByCanonicalHash(ctx context.Context, scope sdkmemory.Scope, conversationID, canonicalHash string) (persistedFact, bool, error) {
	prefix, err := store.conversationPrefix(scope, conversationID)
	if err != nil {
		return persistedFact{}, false, err
	}
	entries, err := store.kv.List(ctx, prefix)
	if err != nil {
		return persistedFact{}, false, fmt.Errorf("fact view: scan exact identities: %w", err)
	}
	for _, entry := range entries {
		_, factID, err := decodeFactKey(prefix, entry.Key)
		if err != nil {
			return persistedFact{}, false, err
		}
		value, ok, err := store.read(ctx, scope, conversationID, factID)
		if err != nil {
			return persistedFact{}, false, err
		}
		if ok && value.Fact.CanonicalHash == canonicalHash {
			return value, true, nil
		}
	}
	return persistedFact{}, false, nil
}

func (store *FactStore) aggregate(ctx context.Context, base persistedFact) (Fact, error) {
	result := cloneFact(base.Fact)
	events, err := store.mergeEvents(ctx, result.Scope, result.ConversationID, result.ID)
	if err != nil {
		return Fact{}, err
	}
	provenance := append([]sdkmemory.SourceRef(nil), result.Provenance...)
	links := append([]string(nil), result.LinkedMemoryIDs...)
	entities := append([]string(nil), result.Entities...)
	for _, event := range events {
		var merge mergeEvent
		if err := decodeJSON(event.Payload, &merge); err != nil {
			return Fact{}, fmt.Errorf("fact view: decode merge event: %w", err)
		}
		if err := validateMergeEvent(merge, result); err != nil {
			return Fact{}, fmt.Errorf("fact view: corrupt merge event: %w", err)
		}
		provenance = append(provenance, merge.Provenance...)
		links = append(links, merge.LinkedMemoryIDs...)
		entities = append(entities, merge.Entities...)
	}
	result.Provenance = uniqueSources(provenance)
	result.LinkedMemoryIDs = normalizeIDs(links, result.ID)
	result.Entities = NormalizeEntities(entities)
	return cloneFact(result), nil
}

func (store *FactStore) writeMergeEvent(ctx context.Context, scope sdkmemory.Scope, conversationID string, event mergeEvent) (string, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return "", fmt.Errorf("fact view: encode merge event: %w", err)
	}
	eventID, err := mergeEventID(data)
	if err != nil {
		return "", err
	}
	stream, err := store.mergeStream(scope, conversationID, event.FactID)
	if err != nil {
		return "", err
	}
	if _, err := store.log.Append(ctx, stream, []storage.Event{{
		Stream:  stream,
		Type:    mergeEventType,
		Payload: data,
	}}, storage.AppendOptions{IdempotencyKey: eventID}); err != nil {
		if errors.Is(err, storage.ErrConflict) {
			return "", errdefs.Conflictf("fact view: merge event %q conflicts", eventID)
		}
		return "", fmt.Errorf("fact view: write merge event %q: %w", eventID, err)
	}
	return eventID, nil
}

func (store *FactStore) mergeEvents(ctx context.Context, scope sdkmemory.Scope, conversationID, factID string) ([]storage.Event, error) {
	stream, err := store.mergeStream(scope, conversationID, factID)
	if err != nil {
		return nil, err
	}
	return store.log.Read(ctx, stream, 0, 0)
}

func (store *FactStore) read(ctx context.Context, scope sdkmemory.Scope, conversationID, factID string) (persistedFact, bool, error) {
	key, err := store.factKey(scope, conversationID, factID)
	if err != nil {
		return persistedFact{}, false, err
	}
	data, err := store.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return persistedFact{}, false, nil
		}
		return persistedFact{}, false, fmt.Errorf("fact view: read fact %q: %w", factID, err)
	}
	var persisted persistedFact
	if err := decodeJSON(data, &persisted); err != nil {
		return persistedFact{}, false, fmt.Errorf("fact view: decode fact %q: %w", factID, err)
	}
	if err := validatePersisted(persisted, scope, conversationID, factID); err != nil {
		return persistedFact{}, false, fmt.Errorf("fact view: corrupt fact %q: %w", factID, err)
	}
	return persisted, true, nil
}

func (store *FactStore) putImmutable(ctx context.Context, key string, data []byte) error {
	put, ok := store.kv.(storage.PutIfAbsentStore)
	if !ok {
		return errors.New("fact view: store must support immutable writes")
	}
	written, err := put.PutIfAbsent(ctx, key, data)
	if err != nil {
		return err
	}
	if written {
		return nil
	}
	existing, err := store.kv.Get(ctx, key)
	if err != nil {
		return err
	}
	if !bytes.Equal(existing, data) {
		return errdefs.Conflictf("fact view: immutable fact %q conflicts", key)
	}
	return nil
}

func (store *FactStore) scopePrefix(scope sdkmemory.Scope) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return "views/fact/v1/" + partition, nil
}

func (store *FactStore) conversationPrefix(scope sdkmemory.Scope, conversationID string) (string, error) {
	prefix, err := store.scopePrefix(scope)
	if err != nil {
		return "", err
	}
	return prefix + "/" + storage.EncodeSegment(conversationID) + "/facts", nil
}

func (store *FactStore) factKey(scope sdkmemory.Scope, conversationID, factID string) (string, error) {
	prefix, err := store.conversationPrefix(scope, conversationID)
	if err != nil {
		return "", err
	}
	return prefix + "/" + storage.EncodeSegment(factID), nil
}

func (store *FactStore) mergeStream(scope sdkmemory.Scope, conversationID, factID string) (string, error) {
	key, err := store.factKey(scope, conversationID, factID)
	if err != nil {
		return "", err
	}
	return key + "/merges", nil
}

func decodeFactKey(prefix, key string) (string, string, error) {
	suffix := strings.TrimPrefix(key, prefix+"/")
	if suffix == key {
		return "", "", errors.New("fact key outside prefix")
	}
	segments := strings.Split(suffix, "/")
	if len(segments) != 1 {
		return "", "", errors.New("fact key has unexpected depth")
	}
	factID, err := storage.DecodeSegment(segments[0])
	if err != nil {
		return "", "", err
	}
	// Recover the conversation from the prefix segments.
	prefixSegments := strings.Split(prefix, "/")
	if len(prefixSegments) < 2 {
		return "", "", errors.New("invalid fact prefix")
	}
	conversationID, err := storage.DecodeSegment(prefixSegments[len(prefixSegments)-2])
	if err != nil {
		return "", "", err
	}
	return conversationID, factID, nil
}

func decodeScopeFactKey(prefix, key string) (string, string, error) {
	suffix := strings.TrimPrefix(key, prefix+"/")
	if suffix == key {
		return "", "", errors.New("fact key outside prefix")
	}
	segments := strings.Split(suffix, "/")
	if len(segments) != 3 || segments[1] != "facts" {
		return "", "", errors.New("fact key has unexpected shape")
	}
	conversationID, err := storage.DecodeSegment(segments[0])
	if err != nil {
		return "", "", err
	}
	factID, err := storage.DecodeSegment(segments[2])
	if err != nil {
		return "", "", err
	}
	return conversationID, factID, nil
}

func mergeEventID(data []byte) (string, error) {
	sum := sha256.Sum256(append([]byte("flowcraft.memory.fact.merge\x00"), data...))
	return hex.EncodeToString(sum[:]), nil
}

func validateMergeEvent(event mergeEvent, fact Fact) error {
	if event.SchemaVersion != schemaVersion || event.FactID != fact.ID || event.CanonicalHash != fact.CanonicalHash {
		return errors.New("merge event identity is invalid")
	}
	if event.EventTime.IsZero() || strings.TrimSpace(event.SourceDigest) == "" || len(event.Provenance) == 0 {
		return errors.New("merge event authority fields are invalid")
	}
	for _, source := range event.Provenance {
		if err := source.Validate(); err != nil {
			return err
		}
	}
	if !reflectStringsEqual(event.Entities, NormalizeEntities(event.Entities)) ||
		!reflectStringsEqual(event.LinkedMemoryIDs, normalizeIDs(event.LinkedMemoryIDs, fact.ID)) {
		return errors.New("merge event state is not canonical")
	}
	return nil
}

func stateContains(current, candidate Fact) bool {
	if candidate.SourceDigest != current.SourceDigest && !sourcesContain(current.Provenance, candidate.Provenance) {
		return false
	}
	return stringsContain(current.Entities, candidate.Entities) &&
		stringsContain(current.LinkedMemoryIDs, normalizeIDs(candidate.LinkedMemoryIDs, current.ID)) &&
		sourcesContain(current.Provenance, candidate.Provenance)
}

func stringsContain(have, want []string) bool {
	set := make(map[string]struct{}, len(have))
	for _, value := range have {
		set[value] = struct{}{}
	}
	for _, value := range want {
		if _, ok := set[value]; !ok {
			return false
		}
	}
	return true
}

func sourcesContain(have, want []sdkmemory.SourceRef) bool {
	for _, target := range want {
		found := false
		for _, value := range have {
			if reflect.DeepEqual(value, target) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func validateAdd(request AddRequest) error {
	if err := validateAddress(request.Scope, request.ConversationID); err != nil {
		return err
	}
	if strings.TrimSpace(request.ID) == "" {
		return errors.New("fact view: fact_id is required")
	}
	if err := request.Content.Validate(); err != nil {
		return fmt.Errorf("fact view: content: %w", err)
	}
	if NormalizeText(request.Content.Text()) == "" {
		return errors.New("fact view: text content is required")
	}
	if request.CanonicalHash != "" && request.CanonicalHash != CanonicalHash(request.Content.Text()) {
		return errors.New("fact view: canonical_hash does not match content")
	}
	if len(request.Provenance) == 0 {
		return errors.New("fact view: provenance is required")
	}
	for index, source := range request.Provenance {
		if err := source.Validate(); err != nil {
			return fmt.Errorf("fact view: provenance %d: %w", index, err)
		}
	}
	return nil
}

func validateAddress(scope sdkmemory.Scope, conversationID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(conversationID) == "" {
		return errors.New("fact view: conversation_id is required")
	}
	return nil
}

func validatePersisted(value persistedFact, scope sdkmemory.Scope, conversationID, factID string) error {
	if value.SchemaVersion != schemaVersion {
		return fmt.Errorf("unsupported schema_version %d", value.SchemaVersion)
	}
	if value.RuntimeID != scope.RuntimeID || value.UserID != scope.UserID ||
		value.AgentID != scope.AgentID ||
		value.ConversationID != conversationID || value.FactID != factID {
		return errors.New("persisted address does not match fact key")
	}
	fact := value.Fact
	if fact.ID != factID || fact.Scope != scope || fact.ConversationID != conversationID ||
		fact.CreatedAt.IsZero() {
		return errors.New("fact address or authority fields are invalid")
	}
	return validateFact(fact)
}

func factTextContent(text string) sdkmessage.Content {
	return sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: text}}}
}

func normalizeIDs(values []string, self string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || value == self {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func uniqueSources(values []sdkmemory.SourceRef) []sdkmemory.SourceRef {
	sortSources(values)
	result := values[:0]
	for _, value := range values {
		if len(result) > 0 && reflect.DeepEqual(result[len(result)-1], value) {
			continue
		}
		result = append(result, value)
	}
	return append([]sdkmemory.SourceRef(nil), result...)
}

func sortSources(values []sdkmemory.SourceRef) {
	sort.Slice(values, func(i, j int) bool {
		left, right := values[i], values[j]
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.ID != right.ID {
			return left.ID < right.ID
		}
		if left.Revision != right.Revision {
			return left.Revision < right.Revision
		}
		return left.Locator < right.Locator
	})
}

func digestSources(values []sdkmemory.SourceRef) string {
	cloned := append([]sdkmemory.SourceRef(nil), values...)
	sortSources(cloned)
	data, _ := json.Marshal(cloned)
	sum := sha256.Sum256(append([]byte("flowcraft.memory.fact.source\x00"), data...))
	return hex.EncodeToString(sum[:])
}

// ComputeSourceDigest independently recomputes the canonical provenance
// digest used by immutable fact records.
func ComputeSourceDigest(values []sdkmemory.SourceRef) string {
	return digestSources(values)
}

func factRevisionDigest(value Fact) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(append([]byte("flowcraft.memory.fact.revision\x00"), data...))
	return hex.EncodeToString(sum[:])
}

func publicationID(scope sdkmemory.Scope, conversationID, factID, event, revisionDigest string) string {
	data, _ := json.Marshal([]any{scope, conversationID, factID, event, revisionDigest})
	sum := sha256.Sum256(append([]byte("flowcraft.memory.fact.publication\x00"), data...))
	return hex.EncodeToString(sum[:])
}

func decodeJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
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
