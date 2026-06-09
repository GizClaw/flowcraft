package message

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// Option configures a WorkspaceStore.
type Option func(*WorkspaceStore)

// WithClock sets the clock used when appending messages without CreatedAt.
func WithClock(clock func() time.Time) Option {
	return func(s *WorkspaceStore) {
		if clock != nil {
			s.clock = clock
		}
	}
}

// WorkspaceStore persists messages as JSONL files in a workspace.
//
// Concurrent writes to the same scoped workspace must go through one
// WorkspaceStore instance. Cross-instance or cross-process writers require an
// external lock or a workspace backend with stronger concurrency guarantees.
type WorkspaceStore struct {
	ws     workspace.Workspace
	clock  func() time.Time
	nextID atomic.Uint64

	mu            sync.Mutex
	conversations map[string]*sync.RWMutex
}

var _ Store = (*WorkspaceStore)(nil)

// NewWorkspaceStore returns a workspace-backed message Store. Callers that
// share a scoped workspace for concurrent writes must share one store instance,
// or provide an external lock / stronger workspace backend.
func NewWorkspaceStore(ws workspace.Workspace, opts ...Option) *WorkspaceStore {
	s := &WorkspaceStore{
		ws:            ws,
		clock:         time.Now,
		conversations: make(map[string]*sync.RWMutex),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

// Append stores messages in request order. The store assigns authoritative
// conversation id, ids, sequence numbers, and missing timestamps.
func (s *WorkspaceStore) Append(ctx context.Context, req AppendRequest) ([]Message, error) {
	if s.ws == nil {
		return nil, errdefs.Validationf("message source: workspace is required")
	}
	if req.ConversationID == "" {
		return nil, errdefs.Validationf("message source: conversation_id is required")
	}
	if len(req.Messages) == 0 {
		return nil, nil
	}

	mu := s.conversationMutex(req.ConversationID)
	mu.Lock()
	defer mu.Unlock()

	usedIDs, maxSeq, err := s.conversationStats(ctx, req.ConversationID, len(req.Messages))
	if err != nil {
		return nil, err
	}

	staged := make([]Message, 0, len(req.Messages))
	for _, msg := range req.Messages {
		if msg.ConversationID == "" {
			msg.ConversationID = req.ConversationID
		}
		if msg.ConversationID != req.ConversationID {
			return nil, errdefs.Validationf("message source: message conversation_id %q conflicts with request conversation_id %q", msg.ConversationID, req.ConversationID)
		}
		if msg.ID != "" {
			if _, exists := usedIDs[msg.ID]; exists {
				return nil, errdefs.Conflictf("message source: duplicate message id %q", msg.ID)
			}
			usedIDs[msg.ID] = struct{}{}
		}
		staged = append(staged, cloneMessage(msg))
	}

	var buf bytes.Buffer
	out := make([]Message, 0, len(staged))
	for _, msg := range staged {
		maxSeq++
		if msg.ID == "" {
			msg.ID = s.nextMessageID(maxSeq, usedIDs)
		}
		if msg.CreatedAt.IsZero() {
			msg.CreatedAt = s.clock()
		}
		msg.ConversationID = req.ConversationID
		msg.Seq = maxSeq

		raw, err := json.Marshal(msg)
		if err != nil {
			return nil, fmt.Errorf("message source: marshal message %q: %w", msg.ID, err)
		}
		buf.Write(raw)
		buf.WriteByte('\n')
		out = append(out, cloneMessage(msg))
	}

	if err := s.ws.Append(ctx, s.messagesPath(req.ConversationID), buf.Bytes()); err != nil {
		return nil, fmt.Errorf("message source: append conversation %q: %w", req.ConversationID, err)
	}
	return out, nil
}

// Get returns one message by conversation and message id.
func (s *WorkspaceStore) Get(ctx context.Context, conversationID, messageID string) (Message, bool, error) {
	if s.ws == nil {
		return Message{}, false, errdefs.Validationf("message source: workspace is required")
	}
	if conversationID == "" || messageID == "" {
		return Message{}, false, nil
	}

	mu := s.conversationMutex(conversationID)
	mu.RLock()
	defer mu.RUnlock()

	var found Message
	ok := false
	err := s.scanConversation(ctx, conversationID, func(_ int, msg Message) (bool, error) {
		if msg.ID == messageID {
			found = cloneMessage(msg)
			ok = true
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return Message{}, false, err
	}
	return found, ok, nil
}

// List returns messages ordered by ascending sequence number.
func (s *WorkspaceStore) List(ctx context.Context, conversationID string, opts ListOptions) ([]Message, error) {
	if s.ws == nil {
		return nil, errdefs.Validationf("message source: workspace is required")
	}
	if conversationID == "" {
		return nil, nil
	}

	mu := s.conversationMutex(conversationID)
	mu.RLock()
	defer mu.RUnlock()

	out := make([]Message, 0)
	err := s.scanConversation(ctx, conversationID, func(_ int, msg Message) (bool, error) {
		if msg.Seq <= opts.AfterSeq {
			return true, nil
		}
		out = append(out, cloneMessage(msg))
		if opts.Limit > 0 && len(out) >= opts.Limit {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListConversations returns conversation IDs in ascending original-ID order.
func (s *WorkspaceStore) ListConversations(ctx context.Context) ([]string, error) {
	if s.ws == nil {
		return nil, errdefs.Validationf("message source: workspace is required")
	}

	entries, err := s.ws.List(ctx, "conversations")
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
		if !strings.HasPrefix(entry.Name(), "k_") {
			continue
		}
		id, err := conversationIDFromPathSegment(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("message source: decode conversation path %q: %w", entry.Name(), err)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// DeleteConversation removes all persisted data for a conversation.
func (s *WorkspaceStore) DeleteConversation(ctx context.Context, conversationID string) error {
	if s.ws == nil {
		return errdefs.Validationf("message source: workspace is required")
	}
	if conversationID == "" {
		return nil
	}

	mu := s.conversationMutex(conversationID)
	mu.Lock()
	defer mu.Unlock()

	if err := s.ws.RemoveAll(ctx, s.conversationDir(conversationID)); err != nil {
		return fmt.Errorf("message source: delete conversation %q: %w", conversationID, err)
	}

	return nil
}

func (s *WorkspaceStore) conversationMutex(conversationID string) *sync.RWMutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	mu, ok := s.conversations[conversationID]
	if !ok {
		mu = &sync.RWMutex{}
		s.conversations[conversationID] = mu
	}
	return mu
}

func (s *WorkspaceStore) conversationStats(ctx context.Context, conversationID string, extraIDs int) (map[string]struct{}, uint64, error) {
	usedIDs := make(map[string]struct{}, extraIDs)
	var maxSeq uint64
	err := s.scanConversation(ctx, conversationID, func(_ int, msg Message) (bool, error) {
		if msg.ID != "" {
			usedIDs[msg.ID] = struct{}{}
		}
		if msg.Seq > maxSeq {
			maxSeq = msg.Seq
		}
		return true, nil
	})
	if err != nil {
		return nil, 0, err
	}
	return usedIDs, maxSeq, nil
}

func (s *WorkspaceStore) scanConversation(ctx context.Context, conversationID string, yield func(line int, msg Message) (bool, error)) error {
	data, err := s.ws.Read(ctx, s.messagesPath(conversationID))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("message source: read conversation %q: %w", conversationID, err)
	}
	if len(data) == 0 {
		return nil
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return fmt.Errorf("message source: decode conversation %q: empty/blank JSONL", conversationID)
	}
	if data[len(data)-1] != '\n' {
		return fmt.Errorf("message source: decode conversation %q: partial JSONL line at end of file", conversationID)
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			return fmt.Errorf("message source: decode conversation %q line %d: blank JSONL line", conversationID, lineNo)
		}
		var msg Message
		if err := json.Unmarshal(line, &msg); err != nil {
			return fmt.Errorf("message source: decode conversation %q line %d: %w", conversationID, lineNo, err)
		}
		keepGoing, err := yield(lineNo, msg)
		if err != nil {
			return err
		}
		if !keepGoing {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("message source: scan conversation %q: %w", conversationID, err)
	}
	return nil
}

func (s *WorkspaceStore) nextMessageID(seq uint64, used map[string]struct{}) string {
	id := fmt.Sprintf("msg-%d", seq)
	if _, exists := used[id]; !exists {
		used[id] = struct{}{}
		return id
	}
	for {
		n := s.nextID.Add(1)
		id = fmt.Sprintf("msg-%d-%d", seq, n)
		if _, exists := used[id]; !exists {
			used[id] = struct{}{}
			return id
		}
	}
}

func (s *WorkspaceStore) conversationDir(conversationID string) string {
	return path.Join("conversations", pathSegment(conversationID))
}

func (s *WorkspaceStore) messagesPath(conversationID string) string {
	return path.Join(s.conversationDir(conversationID), "messages.jsonl")
}

func pathSegment(id string) string {
	return "k_" + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func conversationIDFromPathSegment(segment string) (string, error) {
	if !strings.HasPrefix(segment, "k_") {
		return "", fmt.Errorf("missing k_ prefix")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(segment, "k_"))
	if err != nil {
		return "", err
	}
	id := string(raw)
	if id == "" {
		return "", fmt.Errorf("empty conversation id")
	}
	return id, nil
}
