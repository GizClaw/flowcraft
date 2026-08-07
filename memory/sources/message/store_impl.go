package message

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/storage"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
)

const eventTypeRecord = "message.record"

type persistedRecord struct {
	SchemaVersion  int                `json:"schema_version"`
	Scope          sdkmemory.Scope    `json:"scope"`
	ConversationID string             `json:"conversation_id"`
	Message        sdkmessage.Message `json:"message"`
	Metadata       sdkmemory.Metadata `json:"metadata,omitempty"`
	CreatedAt      time.Time          `json:"created_at"`
}

// Option configures a MessageStore.
type Option func(*MessageStore)

// WithClock replaces the clock used for authoritative timestamps.
func WithClock(clock func() time.Time) Option {
	return func(store *MessageStore) {
		if clock != nil {
			store.clock = clock
		}
	}
}

// MessageStore persists canonical conversation messages on a storage.Log
// (one event per record, batches are idempotent commits). Commit listings
// come from the Log's CommitLog extension, so no separate index can drift
// from the durable batch.
type MessageStore struct {
	log   storage.Log
	clock func() time.Time
}

// NewMessageStore constructs a canonical message store. The log backend must
// expose committed batch metadata through storage.CommitLog.
func NewMessageStore(log storage.Log, options ...Option) (*MessageStore, error) {
	if nilInterface(log) {
		return nil, errors.New("message source: log is required")
	}
	if _, ok := log.(storage.CommitLog); !ok {
		return nil, errors.New("message source: log backend must expose commit metadata (storage.CommitLog)")
	}
	store := &MessageStore{log: log, clock: time.Now}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	return store, nil
}

// Append publishes one immutable commit and returns its records.
func (store *MessageStore) Append(ctx context.Context, request AppendRequest) ([]Record, error) {
	commit, err := store.Commit(ctx, request)
	if err != nil {
		return nil, err
	}
	return cloneRecords(commit.Records), nil
}

// Commit publishes and returns one immutable, versioned turn work item.
func (store *MessageStore) Commit(ctx context.Context, request AppendRequest) (Commit, error) {
	if err := validateAppend(request); err != nil {
		return Commit{}, err
	}
	stream, err := storage.StreamName(request.Scope, request.ConversationID)
	if err != nil {
		return Commit{}, err
	}
	now := store.clock()
	events := make([]storage.Event, len(request.Messages))
	for index, item := range request.Messages {
		payload, err := json.Marshal(persistedRecord{
			SchemaVersion:  messageSchemaVersion,
			Scope:          request.Scope,
			ConversationID: request.ConversationID,
			Message:        item,
			Metadata:       request.Metadata,
			CreatedAt:      now,
		})
		if err != nil {
			return Commit{}, err
		}
		events[index] = storage.Event{
			Stream:    stream,
			Type:      eventTypeRecord,
			Payload:   payload,
			CreatedAt: now,
		}
	}
	logCommit, err := store.log.Append(ctx, stream, events, storage.AppendOptions{
		IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		// The message contract is lenient on retries: replaying an
		// idempotency key with different content returns the original
		// commit, matching the legacy workspace store behavior.
		if errors.Is(err, storage.ErrConflict) {
			return store.commitByKey(ctx, request.Scope, request.ConversationID, stream, request.IdempotencyKey)
		}
		return Commit{}, err
	}
	records, err := store.readRecords(ctx, request.Scope, request.ConversationID, stream,
		logCommit.FirstSeq, logCommit.LastSeq)
	if err != nil {
		return Commit{}, err
	}
	commit := Commit{
		ID:             commitID(request.Scope, request.ConversationID, request.IdempotencyKey),
		Scope:          request.Scope,
		ConversationID: request.ConversationID,
		IdempotencyKey: request.IdempotencyKey,
		Version:        logCommit.LastSeq,
		Records:        records,
		CreatedAt:      records[0].CreatedAt,
	}
	return cloneCommit(commit), nil
}

// Get resolves a canonical message ID directly to its immutable record.
func (store *MessageStore) Get(ctx context.Context, scope sdkmemory.Scope, conversationID, messageID string) (Record, bool, error) {
	if err := validateAddress(scope, conversationID); err != nil {
		return Record{}, false, err
	}
	if strings.TrimSpace(messageID) == "" {
		return Record{}, false, errors.New("message source: message_id is required")
	}
	seq, ok := parseMessageID(messageID)
	if !ok {
		return Record{}, false, nil
	}
	stream, err := storage.StreamName(scope, conversationID)
	if err != nil {
		return Record{}, false, err
	}
	event, err := store.log.ReadAt(ctx, stream, seq)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Record{}, false, nil
		}
		return Record{}, false, err
	}
	record, err := store.recordFromEvent(scope, conversationID, event)
	if err != nil {
		return Record{}, false, err
	}
	return cloneRecord(record), true, nil
}

// List returns records ordered by ascending Seq. AfterSeq is exclusive.
func (store *MessageStore) List(ctx context.Context, scope sdkmemory.Scope, conversationID string, options ListOptions) ([]Record, error) {
	if err := validateAddress(scope, conversationID); err != nil {
		return nil, err
	}
	stream, err := storage.StreamName(scope, conversationID)
	if err != nil {
		return nil, err
	}
	events, err := store.log.Read(ctx, stream, options.AfterSeq, options.Limit)
	if err != nil {
		return nil, err
	}
	return store.recordsFromEvents(scope, conversationID, events)
}

// Latest returns the newest canonical records, ordered by ascending Seq.
func (store *MessageStore) Latest(ctx context.Context, scope sdkmemory.Scope, conversationID string, options LatestOptions) ([]Record, error) {
	if err := validateAddress(scope, conversationID); err != nil {
		return nil, err
	}
	if options.Limit < 0 {
		return nil, errors.New("message source: latest limit must not be negative")
	}
	stream, err := storage.StreamName(scope, conversationID)
	if err != nil {
		return nil, err
	}
	events, err := store.log.ReadLatest(ctx, stream, options.Limit)
	if err != nil {
		return nil, err
	}
	return store.recordsFromEvents(scope, conversationID, events)
}

// ListCommits returns immutable turn work items ordered by ascending Version.
func (store *MessageStore) ListCommits(ctx context.Context, scope sdkmemory.Scope, conversationID string, options ListCommitOptions) ([]Commit, error) {
	if err := validateAddress(scope, conversationID); err != nil {
		return nil, err
	}
	stream, err := storage.StreamName(scope, conversationID)
	if err != nil {
		return nil, err
	}
	commitLog, ok := store.log.(storage.CommitLog)
	if !ok {
		return nil, errors.New("message source: log backend must expose commit metadata (storage.CommitLog)")
	}
	metadata, err := commitLog.ListCommits(ctx, stream, options.AfterVersion, options.Limit)
	if err != nil {
		return nil, err
	}
	commits := make([]Commit, 0, len(metadata))
	for _, item := range metadata {
		records, err := store.readRecords(ctx, scope, conversationID, stream, item.FirstSeq, item.LastSeq)
		if err != nil {
			return nil, err
		}
		commits = append(commits, Commit{
			ID:             commitID(scope, conversationID, item.IdempotencyKey),
			Scope:          scope,
			ConversationID: conversationID,
			IdempotencyKey: item.IdempotencyKey,
			Version:        item.LastSeq,
			Records:        records,
			CreatedAt:      records[0].CreatedAt,
		})
	}
	return commits, nil
}

// ListConversations returns non-empty conversation IDs in lexical order.
func (store *MessageStore) ListConversations(ctx context.Context, scope sdkmemory.Scope) ([]string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return nil, err
	}
	streams, err := store.log.ListStreams(ctx, partition)
	if err != nil {
		return nil, err
	}
	conversations := make([]string, 0, len(streams))
	for _, stream := range streams {
		suffix := strings.TrimPrefix(stream, partition+"/")
		if suffix == stream {
			return nil, fmt.Errorf("message source: stream %q outside partition", stream)
		}
		conversationID, err := storage.DecodeSegment(suffix)
		if err != nil {
			return nil, fmt.Errorf("message source: decode conversation %q: %w", suffix, err)
		}
		conversations = append(conversations, conversationID)
	}
	sort.Strings(conversations)
	return conversations, nil
}

// commitByKey resolves the original commit for an idempotency key. It is the
// retry path: the Log rejected a replayed key with different content, so the
// original commit is authoritative.
func (store *MessageStore) commitByKey(ctx context.Context, scope sdkmemory.Scope, conversationID, stream, key string) (Commit, error) {
	commitLog, ok := store.log.(storage.CommitLog)
	if !ok {
		return Commit{}, errors.New("message source: log backend must expose commit metadata (storage.CommitLog)")
	}
	metadata, found, err := commitLog.ReadCommitByKey(ctx, stream, key)
	if err != nil {
		return Commit{}, err
	}
	if !found {
		return Commit{}, fmt.Errorf("message source: commit for key %q is missing", key)
	}
	records, err := store.readRecords(ctx, scope, conversationID, stream, metadata.FirstSeq, metadata.LastSeq)
	if err != nil {
		return Commit{}, err
	}
	commit := Commit{
		ID:             commitID(scope, conversationID, key),
		Scope:          scope,
		ConversationID: conversationID,
		IdempotencyKey: key,
		Version:        metadata.LastSeq,
		Records:        records,
		CreatedAt:      records[0].CreatedAt,
	}
	return cloneCommit(commit), nil
}

func (store *MessageStore) readRecords(ctx context.Context, scope sdkmemory.Scope, conversationID, stream string, first, last uint64) ([]Record, error) {
	events, err := store.log.Read(ctx, stream, first-1, int(last-first+1))
	if err != nil {
		return nil, err
	}
	if uint64(len(events)) != last-first+1 {
		return nil, fmt.Errorf("message source: commit records %d..%d are incomplete", first, last)
	}
	return store.recordsFromEvents(scope, conversationID, events)
}

func (store *MessageStore) recordsFromEvents(scope sdkmemory.Scope, conversationID string, events []storage.Event) ([]Record, error) {
	records := make([]Record, len(events))
	for index, event := range events {
		record, err := store.recordFromEvent(scope, conversationID, event)
		if err != nil {
			return nil, err
		}
		records[index] = record
	}
	return records, nil
}

func (store *MessageStore) recordFromEvent(scope sdkmemory.Scope, conversationID string, event storage.Event) (Record, error) {
	if event.Type != eventTypeRecord {
		return Record{}, fmt.Errorf("message source: unexpected event type %q", event.Type)
	}
	var value persistedRecord
	if err := decodeStrict(event.Payload, &value); err != nil {
		return Record{}, fmt.Errorf("message source: decode record %d: %w", event.Seq, err)
	}
	if value.SchemaVersion != messageSchemaVersion || value.Scope != scope ||
		value.ConversationID != conversationID {
		return Record{}, fmt.Errorf("message source: corrupt record %d", event.Seq)
	}
	if err := value.Message.Validate(); err != nil {
		return Record{}, fmt.Errorf("message source: corrupt record %d: %w", event.Seq, err)
	}
	return Record{
		ID:             messageID(event.Seq),
		Scope:          scope,
		ConversationID: conversationID,
		Seq:            event.Seq,
		Message:        value.Message.Clone(),
		Metadata:       value.Metadata.Clone(),
		CreatedAt:      value.CreatedAt,
	}, nil
}

func decodeStrict(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("message source: unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func nilInterface(value any) bool {
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
