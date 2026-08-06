package message

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

const messageSchemaVersion = 1

// Option configures a WorkspaceStore.
type Option func(*WorkspaceStore)

// WithClock replaces the clock used for authoritative timestamps.
func WithClock(clock func() time.Time) Option {
	return func(store *WorkspaceStore) {
		if clock != nil {
			store.clock = clock
		}
	}
}

// WorkspaceStore persists every idempotent turn as an immutable commit file.
// Calls through one store are safe for concurrent use. All writers in one
// process that share a workspace must share the same WorkspaceStore; the
// workspace API has no cross-instance or cross-process compare-and-swap.
type WorkspaceStore struct {
	ws    workspace.Workspace
	clock func() time.Time
	mu    sync.RWMutex
}

type persistedCommit struct {
	SchemaVersion  int      `json:"schema_version"`
	RuntimeID      string   `json:"runtime_id"`
	UserID         string   `json:"user_id"`
	AgentID        string   `json:"agent_id,omitempty"`
	ConversationID string   `json:"conversation_id"`
	IdempotencyKey string   `json:"idempotency_key"`
	Records        []Record `json:"records"`
}

type conversationHead struct {
	SchemaVersion  int             `json:"schema_version"`
	Scope          sdkmemory.Scope `json:"scope"`
	ConversationID string          `json:"conversation_id"`
	LastSeq        uint64          `json:"last_seq"`
	LastVersion    uint64          `json:"last_version"`
}

type pendingCommit struct {
	SchemaVersion  int             `json:"schema_version"`
	Scope          sdkmemory.Scope `json:"scope"`
	ConversationID string          `json:"conversation_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	FirstSeq       uint64          `json:"first_seq"`
	LastSeq        uint64          `json:"last_seq"`
}

type commitLocator struct {
	SchemaVersion  int    `json:"schema_version"`
	IdempotencyKey string `json:"idempotency_key"`
	Version        uint64 `json:"version"`
}

var _ Store = (*WorkspaceStore)(nil)

// NewWorkspaceStore constructs a canonical message store.
func NewWorkspaceStore(ws workspace.Workspace, options ...Option) (*WorkspaceStore, error) {
	if nilWorkspace(ws) {
		return nil, errors.New("message source: workspace is required")
	}
	store := &WorkspaceStore{ws: ws, clock: time.Now}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	return store, nil
}

// Append publishes one immutable commit. Idempotency keys are scoped to the
// hard partition and conversation; retrying a key returns that commit.
func (store *WorkspaceStore) Append(ctx context.Context, request AppendRequest) ([]Record, error) {
	commit, err := store.Commit(ctx, request)
	if err != nil {
		return nil, err
	}
	return cloneRecords(commit.Records), nil
}

// Commit publishes and returns one immutable, versioned turn work item.
func (store *WorkspaceStore) Commit(ctx context.Context, request AppendRequest) (Commit, error) {
	if err := validateAppend(request); err != nil {
		return Commit{}, err
	}
	messages := sdkmessage.CloneMessages(request.Messages)
	metadata := request.Metadata.Clone()

	store.mu.Lock()
	defer store.mu.Unlock()

	if err := store.prepareConversation(ctx, request.Scope, request.ConversationID); err != nil {
		return Commit{}, err
	}
	commit, ok, err := store.readCommit(ctx, request.Scope, request.ConversationID, request.IdempotencyKey)
	if err != nil {
		return Commit{}, err
	}
	if ok {
		if err := store.publishCommit(ctx, commit); err != nil {
			return Commit{}, err
		}
		return cloneCommit(commitFromPersisted(commit)), nil
	}
	head, _, err := store.readHead(ctx, request.Scope, request.ConversationID)
	if err != nil {
		return Commit{}, err
	}
	committed := make([]Record, len(messages))
	for index, item := range messages {
		seq := head.LastSeq + uint64(index) + 1
		committed[index] = Record{
			ID:             fmt.Sprintf("msg-%020d", seq),
			Scope:          request.Scope,
			ConversationID: request.ConversationID,
			Seq:            seq,
			Message:        item,
			Metadata:       metadata.Clone(),
			CreatedAt:      store.clock(),
		}
	}
	commit = persistedCommit{
		SchemaVersion:  messageSchemaVersion,
		RuntimeID:      request.Scope.RuntimeID,
		UserID:         request.Scope.UserID,
		AgentID:        request.Scope.AgentID,
		ConversationID: request.ConversationID,
		IdempotencyKey: request.IdempotencyKey,
		Records:        committed,
	}
	pending := pendingCommit{
		SchemaVersion: messageSchemaVersion, Scope: request.Scope,
		ConversationID: request.ConversationID, IdempotencyKey: request.IdempotencyKey,
		FirstSeq: committed[0].Seq, LastSeq: committed[len(committed)-1].Seq,
	}
	if err := store.writeJSON(ctx, store.pendingPath(request.Scope, request.ConversationID), pending); err != nil {
		return Commit{}, fmt.Errorf("message source: reserve commit %q: %w", request.IdempotencyKey, err)
	}
	if err := store.writeCommit(ctx, request.Scope, request.ConversationID, request.IdempotencyKey, commit); err != nil {
		return Commit{}, err
	}
	if err := store.publishCommit(ctx, commit); err != nil {
		return Commit{}, err
	}
	return cloneCommit(commitFromPersisted(commit)), nil
}

// Get resolves a canonical message ID directly to its immutable record.
func (store *WorkspaceStore) Get(ctx context.Context, scope sdkmemory.Scope, conversationID, messageID string) (Record, bool, error) {
	if err := validateAddress(scope, conversationID); err != nil {
		return Record{}, false, err
	}
	if strings.TrimSpace(messageID) == "" {
		return Record{}, false, errors.New("message source: message_id is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.prepareConversation(ctx, scope, conversationID); err != nil {
		return Record{}, false, err
	}
	seq, ok := parseMessageID(messageID)
	if !ok {
		return Record{}, false, nil
	}
	record, ok, err := store.readRecord(ctx, scope, conversationID, seq)
	if err != nil {
		return Record{}, false, err
	}
	if !ok {
		return Record{}, false, nil
	}
	return cloneRecord(record), true, nil
}

// List returns records ordered by ascending Seq. AfterSeq is exclusive.
func (store *WorkspaceStore) List(ctx context.Context, scope sdkmemory.Scope, conversationID string, options ListOptions) ([]Record, error) {
	if err := validateAddress(scope, conversationID); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.prepareConversation(ctx, scope, conversationID); err != nil {
		return nil, err
	}
	head, ok, err := store.readHead(ctx, scope, conversationID)
	if err != nil {
		return nil, err
	}
	if !ok || options.AfterSeq >= head.LastSeq {
		return []Record{}, nil
	}
	result := make([]Record, 0)
	for seq := options.AfterSeq + 1; seq <= head.LastSeq; seq++ {
		record, found, err := store.readRecord(ctx, scope, conversationID, seq)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("message source: indexed record %d is missing", seq)
		}
		result = append(result, record)
		if options.Limit > 0 && len(result) == options.Limit {
			break
		}
	}
	return result, nil
}

// Latest returns the newest canonical records, ordered by ascending Seq.
func (store *WorkspaceStore) Latest(ctx context.Context, scope sdkmemory.Scope, conversationID string, options LatestOptions) ([]Record, error) {
	if err := validateAddress(scope, conversationID); err != nil {
		return nil, err
	}
	if options.Limit < 0 {
		return nil, errors.New("message source: latest limit must not be negative")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.prepareConversation(ctx, scope, conversationID); err != nil {
		return nil, err
	}
	head, ok, err := store.readHead(ctx, scope, conversationID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []Record{}, nil
	}
	start := uint64(1)
	if options.Limit > 0 && uint64(options.Limit) < head.LastSeq {
		start = head.LastSeq - uint64(options.Limit) + 1
	}
	result := make([]Record, 0, head.LastSeq-start+1)
	for seq := start; seq <= head.LastSeq; seq++ {
		record, found, err := store.readRecord(ctx, scope, conversationID, seq)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("message source: indexed record %d is missing", seq)
		}
		result = append(result, record)
	}
	return result, nil
}

// ListCommits returns immutable turn work items ordered by ascending Version.
func (store *WorkspaceStore) ListCommits(ctx context.Context, scope sdkmemory.Scope, conversationID string, options ListCommitOptions) ([]Commit, error) {
	if err := validateAddress(scope, conversationID); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.prepareConversation(ctx, scope, conversationID); err != nil {
		return nil, err
	}
	entries, err := store.ws.List(ctx, store.commitLocatorsDir(scope, conversationID))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return []Commit{}, nil
		}
		return nil, fmt.Errorf("message source: list commit locators: %w", err)
	}
	versions := make([]uint64, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		version, err := strconv.ParseUint(strings.TrimSuffix(entry.Name(), ".json"), 10, 64)
		if err != nil || fmt.Sprintf("%020d.json", version) != entry.Name() {
			return nil, fmt.Errorf("message source: invalid commit locator %q", entry.Name())
		}
		if version > options.AfterVersion {
			versions = append(versions, version)
		}
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	result := make([]Commit, 0)
	for _, version := range versions {
		locator, err := store.readCommitLocator(ctx, scope, conversationID, version)
		if err != nil {
			return nil, err
		}
		commit, ok, err := store.readCommit(ctx, scope, conversationID, locator.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		if !ok || commitFromPersisted(commit).Version != version {
			return nil, fmt.Errorf("message source: commit locator %d is corrupt", version)
		}
		result = append(result, commitFromPersisted(commit))
		if options.Limit > 0 && len(result) == options.Limit {
			break
		}
	}
	return result, nil
}

// ListConversations returns non-empty conversation IDs in lexical order.
func (store *WorkspaceStore) ListConversations(ctx context.Context, scope sdkmemory.Scope) ([]string, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	entries, err := store.ws.List(ctx, store.conversationsDir(scope))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("message source: list conversations: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id, dataName, err := decodeSegment(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("message source: decode conversation path %q: %w", entry.Name(), err)
		}
		if !dataName {
			continue
		}
		if err := store.prepareConversation(ctx, scope, id); err != nil {
			return nil, err
		}
		_, ok, err := store.readHead(ctx, scope, id)
		if err != nil {
			return nil, err
		}
		if ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (store *WorkspaceStore) prepareConversation(ctx context.Context, scope sdkmemory.Scope, conversationID string) error {
	pending, hasPending, err := store.readPending(ctx, scope, conversationID)
	if err != nil {
		return err
	}
	if hasPending {
		commit, ok, err := store.readCommit(ctx, scope, conversationID, pending.IdempotencyKey)
		if err != nil {
			return err
		}
		if ok {
			if err := store.publishCommit(ctx, commit); err != nil {
				return err
			}
		} else if err := store.deleteIfExists(ctx, store.pendingPath(scope, conversationID)); err != nil {
			return fmt.Errorf("message source: discard incomplete reservation: %w", err)
		}
	}
	if _, ok, err := store.readHead(ctx, scope, conversationID); err != nil || ok {
		return err
	}
	commits, err := store.scanCommits(ctx, scope, conversationID)
	if err != nil {
		return err
	}
	for _, commit := range commits {
		persisted, ok, err := store.readCommit(ctx, scope, conversationID, commit.IdempotencyKey)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("message source: legacy commit %q disappeared", commit.ID)
		}
		if err := store.publishCommit(ctx, persisted); err != nil {
			return fmt.Errorf("message source: migrate legacy conversation %q: %w", conversationID, err)
		}
	}
	return nil
}

func (store *WorkspaceStore) publishCommit(ctx context.Context, commit persistedCommit) error {
	value := commitFromPersisted(commit)
	for _, record := range commit.Records {
		if err := store.writeImmutableJSON(ctx, store.recordPath(value.Scope, value.ConversationID, record.Seq), record); err != nil {
			return fmt.Errorf("message source: publish record %q: %w", record.ID, err)
		}
	}
	locator := commitLocator{
		SchemaVersion:  messageSchemaVersion,
		IdempotencyKey: commit.IdempotencyKey,
		Version:        value.Version,
	}
	if err := store.writeImmutableJSON(ctx, store.commitLocatorPath(value.Scope, value.ConversationID, value.Version), locator); err != nil {
		return fmt.Errorf("message source: publish commit locator %q: %w", value.ID, err)
	}
	head, ok, err := store.readHead(ctx, value.Scope, value.ConversationID)
	if err != nil {
		return err
	}
	if ok && head.LastVersion > value.Version {
		return store.deleteIfExists(ctx, store.pendingPath(value.Scope, value.ConversationID))
	}
	if ok && head.LastSeq >= value.Version && head.LastVersion != value.Version {
		return errors.New("message source: head conflicts with commit version")
	}
	head = conversationHead{
		SchemaVersion: messageSchemaVersion, Scope: value.Scope,
		ConversationID: value.ConversationID, LastSeq: value.Version, LastVersion: value.Version,
	}
	if err := store.writeJSON(ctx, store.headPath(value.Scope, value.ConversationID), head); err != nil {
		return fmt.Errorf("message source: publish head: %w", err)
	}
	return store.deleteIfExists(ctx, store.pendingPath(value.Scope, value.ConversationID))
}

func (store *WorkspaceStore) readHead(ctx context.Context, scope sdkmemory.Scope, conversationID string) (conversationHead, bool, error) {
	var head conversationHead
	ok, err := store.readJSON(ctx, store.headPath(scope, conversationID), &head)
	if err != nil || !ok {
		return conversationHead{}, ok, err
	}
	if head.SchemaVersion != messageSchemaVersion || head.Scope != scope ||
		head.ConversationID != conversationID || head.LastSeq == 0 || head.LastVersion == 0 ||
		head.LastVersion > head.LastSeq {
		return conversationHead{}, false, errors.New("message source: corrupt conversation head")
	}
	return head, true, nil
}

func (store *WorkspaceStore) readPending(ctx context.Context, scope sdkmemory.Scope, conversationID string) (pendingCommit, bool, error) {
	var pending pendingCommit
	ok, err := store.readJSON(ctx, store.pendingPath(scope, conversationID), &pending)
	if err != nil || !ok {
		return pendingCommit{}, ok, err
	}
	if pending.SchemaVersion != messageSchemaVersion || pending.Scope != scope ||
		pending.ConversationID != conversationID || pending.IdempotencyKey == "" ||
		pending.FirstSeq == 0 || pending.LastSeq < pending.FirstSeq {
		return pendingCommit{}, false, errors.New("message source: corrupt pending commit")
	}
	return pending, true, nil
}

func (store *WorkspaceStore) readRecord(ctx context.Context, scope sdkmemory.Scope, conversationID string, seq uint64) (Record, bool, error) {
	var record Record
	ok, err := store.readJSON(ctx, store.recordPath(scope, conversationID, seq), &record)
	if err != nil || !ok {
		return Record{}, ok, err
	}
	if record.Scope != scope || record.ConversationID != conversationID || record.Seq != seq ||
		record.ID != messageID(seq) {
		return Record{}, false, fmt.Errorf("message source: corrupt record locator %d", seq)
	}
	if err := record.Message.Validate(); err != nil {
		return Record{}, false, fmt.Errorf("message source: corrupt record %d: %w", seq, err)
	}
	return record, true, nil
}

func (store *WorkspaceStore) readCommitLocator(ctx context.Context, scope sdkmemory.Scope, conversationID string, version uint64) (commitLocator, error) {
	var locator commitLocator
	ok, err := store.readJSON(ctx, store.commitLocatorPath(scope, conversationID, version), &locator)
	if err != nil {
		return commitLocator{}, err
	}
	if !ok || locator.SchemaVersion != messageSchemaVersion ||
		locator.IdempotencyKey == "" || locator.Version != version {
		return commitLocator{}, fmt.Errorf("message source: corrupt commit locator %d", version)
	}
	return locator, nil
}

func messageID(seq uint64) string {
	return fmt.Sprintf("msg-%020d", seq)
}

func parseMessageID(id string) (uint64, bool) {
	if !strings.HasPrefix(id, "msg-") || len(id) != len("msg-")+20 {
		return 0, false
	}
	seq, err := strconv.ParseUint(strings.TrimPrefix(id, "msg-"), 10, 64)
	return seq, err == nil && seq > 0 && messageID(seq) == id
}

func (store *WorkspaceStore) scanConversation(ctx context.Context, scope sdkmemory.Scope, conversationID string) ([]Record, error) {
	commits, err := store.scanCommits(ctx, scope, conversationID)
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0)
	for _, commit := range commits {
		records = append(records, cloneRecords(commit.Records)...)
	}
	for index, record := range records {
		want := uint64(index + 1)
		if record.Seq != want {
			return nil, fmt.Errorf("message source: corrupt conversation %q: sequence %d, want %d", conversationID, record.Seq, want)
		}
	}
	return records, nil
}

func (store *WorkspaceStore) scanCommits(ctx context.Context, scope sdkmemory.Scope, conversationID string) ([]Commit, error) {
	entries, err := store.ws.List(ctx, store.commitsDir(scope, conversationID))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return []Commit{}, nil
		}
		return nil, fmt.Errorf("message source: list conversation %q commits: %w", conversationID, err)
	}
	commits := make([]Commit, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		key, dataName, err := decodeDataFilename(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("message source: decode commit filename %q: %w", entry.Name(), err)
		}
		if !dataName {
			continue
		}
		commit, ok, err := store.readCommit(ctx, scope, conversationID, key)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("message source: commit %q disappeared during scan", key)
		}
		commits = append(commits, commitFromPersisted(commit))
	}
	sort.Slice(commits, func(i, j int) bool { return commits[i].Version < commits[j].Version })
	expected := uint64(1)
	for _, commit := range commits {
		for _, record := range commit.Records {
			if record.Seq != expected {
				return nil, fmt.Errorf("message source: corrupt conversation %q: sequence %d, want %d", conversationID, record.Seq, expected)
			}
			expected++
		}
	}
	return commits, nil
}

func (store *WorkspaceStore) readCommit(ctx context.Context, scope sdkmemory.Scope, conversationID, key string) (persistedCommit, bool, error) {
	data, err := store.ws.Read(ctx, store.commitPath(scope, conversationID, key))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return persistedCommit{}, false, nil
		}
		return persistedCommit{}, false, fmt.Errorf("message source: read commit %q: %w", key, err)
	}
	var commit persistedCommit
	if err := decodeJSON(data, &commit); err != nil {
		return persistedCommit{}, false, fmt.Errorf("message source: decode commit %q: %w", key, err)
	}
	if err := validateCommit(commit, scope, conversationID, key); err != nil {
		return persistedCommit{}, false, fmt.Errorf("message source: corrupt commit %q: %w", key, err)
	}
	return commit, true, nil
}

func validateCommit(commit persistedCommit, scope sdkmemory.Scope, conversationID, key string) error {
	if commit.SchemaVersion != messageSchemaVersion {
		return fmt.Errorf("unsupported schema_version %d", commit.SchemaVersion)
	}
	if commit.RuntimeID != scope.RuntimeID || commit.UserID != scope.UserID || commit.AgentID != scope.AgentID ||
		commit.ConversationID != conversationID || commit.IdempotencyKey != key {
		return errors.New("persisted address does not match workspace path")
	}
	if len(commit.Records) == 0 {
		return errors.New("commit records are required")
	}
	previousSeq := uint64(0)
	for index, record := range commit.Records {
		if record.ID == "" || record.Seq <= previousSeq || record.ConversationID != conversationID {
			return fmt.Errorf("invalid record at index %d", index)
		}
		if record.Scope != scope {
			return fmt.Errorf("record %q crosses hard partition", record.ID)
		}
		if record.CreatedAt.IsZero() {
			return fmt.Errorf("record %q has zero created_at", record.ID)
		}
		if err := record.Message.Validate(); err != nil {
			return fmt.Errorf("record %q message: %w", record.ID, err)
		}
		previousSeq = record.Seq
	}
	return nil
}

func (store *WorkspaceStore) writeCommit(ctx context.Context, scope sdkmemory.Scope, conversationID, key string, commit persistedCommit) error {
	data, err := json.Marshal(commit)
	if err != nil {
		return fmt.Errorf("message source: encode commit %q: %w", key, err)
	}
	target := store.commitPath(scope, conversationID, key)
	existing, readErr := store.ws.Read(ctx, target)
	if readErr == nil {
		if !bytes.Equal(existing, data) {
			return errdefs.Conflictf("message source: immutable commit %q conflicts", key)
		}
		return nil
	}
	if !errdefs.IsNotFound(readErr) {
		return fmt.Errorf("message source: inspect commit %q: %w", key, readErr)
	}
	if err := workspace.AtomicWrite(ctx, store.ws, target, data); err != nil {
		return fmt.Errorf("message source: write commit %q: %w", key, err)
	}
	return nil
}

func (store *WorkspaceStore) readJSON(ctx context.Context, name string, destination any) (bool, error) {
	data, err := store.ws.Read(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if err := decodeJSON(data, destination); err != nil {
		return false, fmt.Errorf("decode %q: %w", name, err)
	}
	return true, nil
}

func (store *WorkspaceStore) writeJSON(ctx context.Context, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return workspace.AtomicWrite(ctx, store.ws, name, data)
}

func (store *WorkspaceStore) writeImmutableJSON(ctx context.Context, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	existing, err := store.ws.Read(ctx, name)
	if err == nil {
		if !bytes.Equal(existing, data) {
			return errdefs.Conflictf("immutable record %q conflicts", name)
		}
		return nil
	}
	if !errdefs.IsNotFound(err) {
		return err
	}
	return workspace.AtomicWrite(ctx, store.ws, name, data)
}

func (store *WorkspaceStore) deleteIfExists(ctx context.Context, name string) error {
	err := store.ws.Delete(ctx, name)
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func validateAppend(request AppendRequest) error {
	if err := validateAddress(request.Scope, request.ConversationID); err != nil {
		return err
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" {
		return errors.New("message source: idempotency_key is required")
	}
	if len(request.Messages) == 0 {
		return errors.New("message source: messages are required")
	}
	for index, item := range request.Messages {
		if err := item.Validate(); err != nil {
			return fmt.Errorf("message source: message %d: %w", index, err)
		}
	}
	return nil
}

func validateAddress(scope sdkmemory.Scope, conversationID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(conversationID) == "" {
		return errors.New("message source: conversation_id is required")
	}
	return nil
}

func (store *WorkspaceStore) partitionDir(scope sdkmemory.Scope) string {
	return path.Join("sources", "message", "v1", "partitions", encodeSegment(scope.RuntimeID),
		encodeSegment(scope.UserID), encodeSegment(scope.AgentID))
}

func (store *WorkspaceStore) conversationsDir(scope sdkmemory.Scope) string {
	return path.Join(store.partitionDir(scope), "conversations")
}

func (store *WorkspaceStore) commitsDir(scope sdkmemory.Scope, conversationID string) string {
	return path.Join(store.conversationsDir(scope), encodeSegment(conversationID), "commits")
}

func (store *WorkspaceStore) conversationDir(scope sdkmemory.Scope, conversationID string) string {
	return path.Join(store.conversationsDir(scope), encodeSegment(conversationID))
}

func (store *WorkspaceStore) headPath(scope sdkmemory.Scope, conversationID string) string {
	return path.Join(store.conversationDir(scope, conversationID), "head.json")
}

func (store *WorkspaceStore) pendingPath(scope sdkmemory.Scope, conversationID string) string {
	return path.Join(store.conversationDir(scope, conversationID), "pending.json")
}

func (store *WorkspaceStore) recordsDir(scope sdkmemory.Scope, conversationID string) string {
	return path.Join(store.conversationDir(scope, conversationID), "records")
}

func (store *WorkspaceStore) recordPath(scope sdkmemory.Scope, conversationID string, seq uint64) string {
	return path.Join(store.recordsDir(scope, conversationID), fmt.Sprintf("%020d.json", seq))
}

func (store *WorkspaceStore) commitLocatorsDir(scope sdkmemory.Scope, conversationID string) string {
	return path.Join(store.conversationDir(scope, conversationID), "commit-index")
}

func (store *WorkspaceStore) commitLocatorPath(scope sdkmemory.Scope, conversationID string, version uint64) string {
	return path.Join(store.commitLocatorsDir(scope, conversationID), fmt.Sprintf("%020d.json", version))
}

func (store *WorkspaceStore) commitPath(scope sdkmemory.Scope, conversationID, key string) string {
	return path.Join(store.commitsDir(scope, conversationID), encodeSegment(key)+".json")
}

func encodeSegment(value string) string {
	return "k_" + base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeSegment(segment string) (string, bool, error) {
	if !strings.HasPrefix(segment, "k_") {
		return "", false, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(segment, "k_"))
	if err != nil {
		return "", true, err
	}
	if encodeSegment(string(data)) != segment {
		return "", true, errors.New("non-canonical path segment")
	}
	return string(data), true, nil
}

func decodeDataFilename(name string) (string, bool, error) {
	if !strings.HasSuffix(name, ".json") {
		return "", false, nil
	}
	return decodeSegment(strings.TrimSuffix(name, ".json"))
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

func nilWorkspace(ws workspace.Workspace) bool {
	if ws == nil {
		return true
	}
	value := reflect.ValueOf(ws)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
