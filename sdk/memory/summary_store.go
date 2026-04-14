package memory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/textsearch"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/rs/xid"
)

// SummaryNode represents a node in the summary DAG.
type SummaryNode struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	Depth          int       `json:"depth"`
	Content        string    `json:"content"`
	ExpandHint     string    `json:"expand_hint,omitempty"`
	SourceIDs      []string  `json:"source_ids,omitempty"`
	EarliestSeq    int       `json:"earliest_seq"`
	LatestSeq      int       `json:"latest_seq"`
	TokenCount     int       `json:"token_count"`
	CreatedAt      time.Time `json:"created_at"`
	Deleted        bool      `json:"deleted,omitempty"`
}

// SummaryListOptions controls List filtering.
type SummaryListOptions struct {
	Depth  *int
	MinSeq int
	MaxSeq int
	Limit  int
}

// SummaryStore persists and retrieves summary DAG nodes.
// All operations are scoped by conversation ID.
type SummaryStore interface {
	Save(ctx context.Context, node *SummaryNode) error
	GetByConvID(ctx context.Context, convID, id string) (*SummaryNode, error)
	List(ctx context.Context, convID string, opts SummaryListOptions) ([]*SummaryNode, error)
	Search(ctx context.Context, convID, query string, topK int) ([]*SummaryNode, error)
	DeleteByConvID(ctx context.Context, convID, id string) error
	ListAll(ctx context.Context, convID string) ([]*SummaryNode, error)
	Rewrite(ctx context.Context, convID string, nodes []*SummaryNode) error
}

// NewSummaryNodeID generates a unique ID for a summary node.
func NewSummaryNodeID() string {
	return xid.New().String()
}

// FileSummaryStore is a Workspace-backed SummaryStore using JSONL files.
// It caches parsed nodes per conversation to avoid repeated disk reads.
type FileSummaryStore struct {
	ws     workspace.Workspace
	prefix string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	cache map[string][]*SummaryNode
}

// NewFileSummaryStore creates a FileSummaryStore rooted at the given prefix.
func NewFileSummaryStore(ws workspace.Workspace, prefix string) *FileSummaryStore {
	return &FileSummaryStore{
		ws:     ws,
		prefix: prefix,
		locks:  make(map[string]*sync.Mutex),
		cache:  make(map[string][]*SummaryNode),
	}
}

func (s *FileSummaryStore) convMu(convID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.locks[convID]
	if !ok {
		m = &sync.Mutex{}
		s.locks[convID] = m
	}
	return m
}

func (s *FileSummaryStore) summariesPath(convID string) string {
	if s.prefix != "" {
		return fmt.Sprintf("%s/%s/summaries.jsonl", s.prefix, convID)
	}
	return fmt.Sprintf("%s/summaries.jsonl", convID)
}

// loadCached returns cached nodes or reads from disk and populates cache.
// Caller must hold the per-conversation lock.
func (s *FileSummaryStore) loadCached(ctx context.Context, convID string) ([]*SummaryNode, error) {
	s.mu.Lock()
	if nodes, ok := s.cache[convID]; ok {
		s.mu.Unlock()
		return nodes, nil
	}
	s.mu.Unlock()

	nodes, err := s.readFromDisk(ctx, convID)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.cache[convID] = nodes
	s.mu.Unlock()
	return nodes, nil
}

func (s *FileSummaryStore) setCache(convID string, nodes []*SummaryNode) {
	s.mu.Lock()
	s.cache[convID] = nodes
	s.mu.Unlock()
}

func (s *FileSummaryStore) appendCache(convID string, node *SummaryNode) {
	s.mu.Lock()
	s.cache[convID] = append(s.cache[convID], node)
	s.mu.Unlock()
}

func (s *FileSummaryStore) readFromDisk(ctx context.Context, convID string) ([]*SummaryNode, error) {
	path := s.summariesPath(convID)
	exists, err := s.ws.Exists(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("summary_store: check exists %q: %w", path, err)
	}
	if !exists {
		return nil, nil
	}

	data, err := s.ws.Read(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("summary_store: read %q: %w", path, err)
	}

	var nodes []*SummaryNode
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var n SummaryNode
		if err := json.Unmarshal(line, &n); err != nil {
			continue
		}
		nodes = append(nodes, &n)
	}
	return nodes, scanner.Err()
}

func (s *FileSummaryStore) Save(ctx context.Context, node *SummaryNode) error {
	mu := s.convMu(node.ConversationID)
	mu.Lock()
	defer mu.Unlock()

	if node.ID == "" {
		node.ID = NewSummaryNodeID()
	}
	if node.CreatedAt.IsZero() {
		node.CreatedAt = time.Now()
	}

	// Warm cache before disk write so the new node isn't double-counted.
	if _, err := s.loadCached(ctx, node.ConversationID); err != nil {
		return err
	}

	data, err := json.Marshal(node)
	if err != nil {
		return fmt.Errorf("summary_store: marshal: %w", err)
	}
	data = append(data, '\n')

	path := s.summariesPath(node.ConversationID)
	if err := s.ws.Append(ctx, path, data); err != nil {
		return fmt.Errorf("summary_store: append %q: %w", path, err)
	}

	s.appendCache(node.ConversationID, node)
	return nil
}

func (s *FileSummaryStore) GetByConvID(ctx context.Context, convID, id string) (*SummaryNode, error) {
	mu := s.convMu(convID)
	mu.Lock()
	defer mu.Unlock()

	nodes, err := s.loadCached(ctx, convID)
	if err != nil {
		return nil, err
	}

	// Scan in reverse to get the latest version (in case of updates).
	for i := len(nodes) - 1; i >= 0; i-- {
		n := nodes[i]
		if n.ID == id && !n.Deleted {
			return n, nil
		}
		if n.ID == id && n.Deleted {
			return nil, fmt.Errorf("summary_store: node %q deleted", id)
		}
	}
	return nil, fmt.Errorf("summary_store: node %q not found", id)
}

func (s *FileSummaryStore) List(ctx context.Context, convID string, opts SummaryListOptions) ([]*SummaryNode, error) {
	mu := s.convMu(convID)
	mu.Lock()
	defer mu.Unlock()

	allNodes, err := s.loadCached(ctx, convID)
	if err != nil {
		return nil, err
	}

	// Build set of deleted IDs (last occurrence wins).
	deleted := make(map[string]bool)
	latest := make(map[string]*SummaryNode)
	for _, n := range allNodes {
		if n.Deleted {
			deleted[n.ID] = true
		} else {
			deleted[n.ID] = false
			latest[n.ID] = n
		}
	}

	var result []*SummaryNode
	for id, isDel := range deleted {
		if isDel {
			continue
		}
		n := latest[id]
		if opts.Depth != nil && n.Depth != *opts.Depth {
			continue
		}
		if opts.MinSeq > 0 && n.LatestSeq < opts.MinSeq {
			continue
		}
		if opts.MaxSeq > 0 && n.EarliestSeq > opts.MaxSeq {
			continue
		}
		result = append(result, n)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].EarliestSeq < result[j].EarliestSeq
	})

	if opts.Limit > 0 && len(result) > opts.Limit {
		result = result[:opts.Limit]
	}
	return result, nil
}

func (s *FileSummaryStore) Search(ctx context.Context, convID, query string, topK int) ([]*SummaryNode, error) {
	mu := s.convMu(convID)
	mu.Lock()
	defer mu.Unlock()

	allNodes, err := s.loadCached(ctx, convID)
	if err != nil {
		return nil, err
	}

	// Filter out deleted.
	var active []*SummaryNode
	deleted := make(map[string]bool)
	for _, n := range allNodes {
		if n.Deleted {
			deleted[n.ID] = true
		}
	}
	seen := make(map[string]bool)
	for i := len(allNodes) - 1; i >= 0; i-- {
		n := allNodes[i]
		if deleted[n.ID] || seen[n.ID] {
			continue
		}
		seen[n.ID] = true
		active = append(active, n)
	}

	if len(active) == 0 || query == "" {
		return nil, nil
	}

	tokenizer := textsearch.DetectTokenizer(query)
	keywords := textsearch.ExtractKeywords(query, tokenizer)
	if len(keywords) == 0 {
		return nil, nil
	}

	corpus := textsearch.NewCorpusStats()
	var docs [][]string
	for _, n := range active {
		tokens := tokenizer.Tokenize(n.Content + " " + n.ExpandHint)
		corpus.AddDocument(tokens)
		docs = append(docs, tokens)
	}

	type scored struct {
		node  *SummaryNode
		score float64
	}
	var results []scored
	for i, n := range active {
		sc := textsearch.BM25(docs[i], keywords, corpus)
		if sc > 0 {
			results = append(results, scored{node: n, score: sc})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	if topK > 0 && len(results) > topK {
		results = results[:topK]
	}

	out := make([]*SummaryNode, len(results))
	for i, r := range results {
		out[i] = r.node
	}
	return out, nil
}

func (s *FileSummaryStore) DeleteByConvID(ctx context.Context, convID, id string) error {
	mu := s.convMu(convID)
	mu.Lock()
	defer mu.Unlock()

	marker := &SummaryNode{ID: id, ConversationID: convID, Deleted: true}
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := s.summariesPath(convID)
	if err := s.ws.Append(ctx, path, data); err != nil {
		return err
	}

	s.appendCache(convID, marker)
	return nil
}

func (s *FileSummaryStore) ListAll(ctx context.Context, convID string) ([]*SummaryNode, error) {
	mu := s.convMu(convID)
	mu.Lock()
	defer mu.Unlock()
	return s.loadCached(ctx, convID)
}

func (s *FileSummaryStore) Rewrite(ctx context.Context, convID string, nodes []*SummaryNode) error {
	mu := s.convMu(convID)
	mu.Lock()
	defer mu.Unlock()

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, n := range nodes {
		if err := enc.Encode(n); err != nil {
			return fmt.Errorf("summary_store: encode: %w", err)
		}
	}

	path := s.summariesPath(convID)
	if err := s.ws.Write(ctx, path, buf.Bytes()); err != nil {
		return err
	}

	cp := make([]*SummaryNode, len(nodes))
	copy(cp, nodes)
	s.setCache(convID, cp)
	return nil
}
