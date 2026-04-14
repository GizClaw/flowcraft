package memory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/workspace"

	otellog "go.opentelemetry.io/otel/log"
)

// FileStore is a Workspace-backed Store that persists messages as JSONL files.
// Each conversation is stored at {prefix}/{conversationID}/messages.jsonl with
// one JSON-encoded model.Message per line. Saves are incremental.
type FileStore struct {
	ws     workspace.Workspace
	prefix string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewFileStore creates a FileStore rooted at the given prefix directory
// within the workspace (e.g. "memory").
func NewFileStore(ws workspace.Workspace, prefix string) *FileStore {
	return &FileStore{
		ws:     ws,
		prefix: prefix,
		locks:  make(map[string]*sync.Mutex),
	}
}

func (s *FileStore) convMu(conversationID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.locks[conversationID]
	if !ok {
		m = &sync.Mutex{}
		s.locks[conversationID] = m
	}
	return m
}

func (s *FileStore) messagesPath(conversationID string) string {
	return fmt.Sprintf("%s/%s/messages.jsonl", s.prefix, conversationID)
}

func (s *FileStore) GetMessages(ctx context.Context, conversationID string) ([]model.Message, error) {
	mu := s.convMu(conversationID)
	mu.Lock()
	defer mu.Unlock()
	return s.readMessages(ctx, conversationID)
}

func (s *FileStore) readMessages(ctx context.Context, conversationID string) ([]model.Message, error) {
	path := s.messagesPath(conversationID)
	exists, err := s.ws.Exists(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("memory: check exists %q: %w", path, err)
	}
	if !exists {
		return nil, nil
	}

	data, err := s.ws.Read(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("memory: read %q: %w", path, err)
	}

	var msgs []model.Message
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var msg model.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			telemetry.Warn(ctx, "memory: skipping malformed line",
				otellog.String("path", path),
				otellog.Int("line", lineNum),
				otellog.String("error", err.Error()))
			continue
		}
		msgs = append(msgs, msg)
	}
	if err := scanner.Err(); err != nil {
		return msgs, fmt.Errorf("memory: scan %q: %w", path, err)
	}
	return msgs, nil
}

func (s *FileStore) SaveMessages(ctx context.Context, conversationID string, messages []model.Message) error {
	mu := s.convMu(conversationID)
	mu.Lock()
	defer mu.Unlock()

	existing, err := s.readMessages(ctx, conversationID)
	if err != nil {
		return err
	}

	path := s.messagesPath(conversationID)

	if len(existing) == 0 {
		return s.writeAll(ctx, path, messages)
	}
	if len(messages) <= len(existing) {
		if len(messages) < len(existing) {
			return s.writeAll(ctx, path, messages)
		}
		return nil
	}

	newMsgs := messages[len(existing):]
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, msg := range newMsgs {
		if err := enc.Encode(msg); err != nil {
			return fmt.Errorf("memory: encode message: %w", err)
		}
	}
	if err := s.ws.Append(ctx, path, buf.Bytes()); err != nil {
		return fmt.Errorf("memory: append %q: %w", path, err)
	}
	return nil
}

func (s *FileStore) DeleteMessages(ctx context.Context, conversationID string) error {
	mu := s.convMu(conversationID)
	mu.Lock()

	dir := fmt.Sprintf("%s/%s", s.prefix, conversationID)
	if err := s.ws.RemoveAll(ctx, dir); err != nil {
		mu.Unlock()
		return fmt.Errorf("memory: delete conversation %q: %w", conversationID, err)
	}

	// Remove the lock entry while still holding it, so no concurrent caller
	// can observe a deleted-but-unlocked mutex. New callers will create a
	// fresh mutex via convMu().
	s.mu.Lock()
	delete(s.locks, conversationID)
	s.mu.Unlock()
	mu.Unlock()

	return nil
}

// --- SummaryCacheStore implementation ---

func (s *FileStore) summaryPath(conversationID string) string {
	return fmt.Sprintf("%s/%s/summary.json", s.prefix, conversationID)
}

type summaryJSON struct {
	Text     string `json:"text"`
	MsgCount int    `json:"msg_count"`
}

func (s *FileStore) GetSummary(ctx context.Context, conversationID string) (string, int, error) {
	mu := s.convMu(conversationID)
	mu.Lock()
	defer mu.Unlock()

	path := s.summaryPath(conversationID)
	exists, err := s.ws.Exists(ctx, path)
	if err != nil || !exists {
		return "", 0, err
	}
	data, err := s.ws.Read(ctx, path)
	if err != nil {
		return "", 0, fmt.Errorf("memory: read summary cache: %w", err)
	}
	var cache summaryJSON
	if err := json.Unmarshal(data, &cache); err != nil {
		return "", 0, fmt.Errorf("memory: unmarshal summary cache: %w", err)
	}
	return cache.Text, cache.MsgCount, nil
}

func (s *FileStore) SaveSummary(ctx context.Context, conversationID, summary string, msgCount int) error {
	mu := s.convMu(conversationID)
	mu.Lock()
	defer mu.Unlock()

	data, err := json.Marshal(summaryJSON{Text: summary, MsgCount: msgCount})
	if err != nil {
		return fmt.Errorf("memory: marshal summary cache: %w", err)
	}
	path := s.summaryPath(conversationID)
	if err := s.ws.Write(ctx, path, data); err != nil {
		return fmt.Errorf("memory: write summary cache: %w", err)
	}
	return nil
}

// GetMessageRange returns messages in the range [start, end).
func (s *FileStore) GetMessageRange(ctx context.Context, conversationID string, start, end int) ([]model.Message, error) {
	mu := s.convMu(conversationID)
	mu.Lock()
	defer mu.Unlock()

	msgs, err := s.readMessages(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	if start < 0 {
		start = 0
	}
	if end > len(msgs) {
		end = len(msgs)
	}
	if start >= end {
		return nil, nil
	}
	result := make([]model.Message, end-start)
	copy(result, msgs[start:end])
	return result, nil
}

func (s *FileStore) writeAll(ctx context.Context, path string, messages []model.Message) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, msg := range messages {
		if err := enc.Encode(msg); err != nil {
			return fmt.Errorf("memory: encode message: %w", err)
		}
	}
	if err := s.ws.Write(ctx, path, buf.Bytes()); err != nil {
		return fmt.Errorf("memory: write %q: %w", path, err)
	}
	return nil
}
