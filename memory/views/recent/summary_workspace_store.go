package recent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// SummaryWorkspaceStore persists SummaryDAG nodes as JSON files in a workspace.
//
// Each node is stored under:
// conversations/{encodedConversationID}/nodes/{encodedNodeID}.json
//
// Concurrent writes to the same scoped workspace must go through one
// SummaryWorkspaceStore instance. Cross-instance or cross-process writers
// require an external lock or a workspace backend with stronger concurrency
// guarantees.
type SummaryWorkspaceStore struct {
	ws                workspace.Workspace
	pathSegmentPrefix string
	tmpCounter        atomic.Uint64

	mu sync.RWMutex
}

var _ SummaryStore = (*SummaryWorkspaceStore)(nil)

// defaultSummaryPathSegmentPrefix marks encoded workspace path segments. It is
// not part of SummaryNode IDs, conversation IDs, or other business identifiers.
const defaultSummaryPathSegmentPrefix = "sdag_"

// SummaryWorkspaceStoreOption configures a SummaryWorkspaceStore.
type SummaryWorkspaceStoreOption interface {
	applySummaryWorkspaceStore(*SummaryWorkspaceStore)
}

type summaryPathSegmentPrefixOption string

// WithSummaryPathSegmentPrefix sets the encoded workspace path segment marker.
// Empty prefixes are ignored so callers do not accidentally disable the default
// path segment encoding marker.
func WithSummaryPathSegmentPrefix(prefix string) SummaryWorkspaceStoreOption {
	return summaryPathSegmentPrefixOption(prefix)
}

func (o summaryPathSegmentPrefixOption) applySummaryWorkspaceStore(s *SummaryWorkspaceStore) {
	if o != "" {
		s.pathSegmentPrefix = string(o)
	}
}

// NewSummaryWorkspaceStore returns a workspace-backed SummaryDAG store.
func NewSummaryWorkspaceStore(ws workspace.Workspace, opts ...SummaryWorkspaceStoreOption) *SummaryWorkspaceStore {
	s := &SummaryWorkspaceStore{
		ws:                ws,
		pathSegmentPrefix: defaultSummaryPathSegmentPrefix,
	}
	for _, opt := range opts {
		if opt != nil {
			opt.applySummaryWorkspaceStore(s)
		}
	}
	return s
}

// PutNode stores a summary node as the authoritative value for its conversation
// and node id.
func (s *SummaryWorkspaceStore) PutNode(ctx context.Context, node SummaryNode) (SummaryNode, error) {
	if s.ws == nil {
		return SummaryNode{}, errdefs.Validationf("%s: workspace is required", summaryDAGErrPrefix)
	}
	if err := validateSummaryNode(node); err != nil {
		return SummaryNode{}, err
	}

	node = cloneSummaryNode(node)

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writeNode(ctx, node); err != nil {
		return SummaryNode{}, err
	}
	return cloneSummaryNode(node), nil
}

// GetNode returns one summary node by conversation and node id.
func (s *SummaryWorkspaceStore) GetNode(ctx context.Context, conversationID string, id NodeID) (SummaryNode, bool, error) {
	if s.ws == nil {
		return SummaryNode{}, false, errdefs.Validationf("%s: workspace is required", summaryDAGErrPrefix)
	}
	if conversationID == "" || id == "" {
		return SummaryNode{}, false, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	node, ok, err := s.readNode(ctx, conversationID, id)
	if err != nil {
		return SummaryNode{}, false, err
	}
	if !ok {
		return SummaryNode{}, false, nil
	}
	return cloneSummaryNode(node), true, nil
}

// ListNodes returns summary nodes ordered by ascending node id.
func (s *SummaryWorkspaceStore) ListNodes(ctx context.Context, conversationID string, opts ListOptions) ([]SummaryNode, error) {
	if s.ws == nil {
		return nil, errdefs.Validationf("%s: workspace is required", summaryDAGErrPrefix)
	}
	if conversationID == "" {
		return nil, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := s.ws.List(ctx, s.nodesDir(conversationID))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: list conversation %q nodes: %w", summaryDAGErrPrefix, conversationID, err)
	}

	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		segment := strings.TrimSuffix(entry.Name(), ".json")
		if !strings.HasPrefix(segment, s.pathSegmentPrefix) {
			continue
		}
		id, err := s.rawPathSegment(segment)
		if err != nil {
			return nil, fmt.Errorf("%s: decode node id %q: %w", summaryDAGErrPrefix, entry.Name(), err)
		}
		if id > string(opts.AfterID) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	out := make([]SummaryNode, 0, len(ids))
	for _, id := range ids {
		node, ok, err := s.readNode(ctx, conversationID, NodeID(id))
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if opts.Level != nil && node.Level != *opts.Level {
			continue
		}
		out = append(out, cloneSummaryNode(node))
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out, nil
}

// DeleteConversation removes all persisted summary nodes for a conversation. It
// is idempotent.
func (s *SummaryWorkspaceStore) DeleteConversation(ctx context.Context, conversationID string) error {
	if s.ws == nil {
		return errdefs.Validationf("%s: workspace is required", summaryDAGErrPrefix)
	}
	if conversationID == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ws.RemoveAll(ctx, s.conversationDir(conversationID)); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("%s: delete conversation %q: %w", summaryDAGErrPrefix, conversationID, err)
	}
	return nil
}

func (s *SummaryWorkspaceStore) readNode(ctx context.Context, conversationID string, id NodeID) (SummaryNode, bool, error) {
	data, err := s.ws.Read(ctx, s.nodePath(conversationID, id))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return SummaryNode{}, false, nil
		}
		return SummaryNode{}, false, fmt.Errorf("%s: read node %q/%q: %w", summaryDAGErrPrefix, conversationID, id, err)
	}

	var node SummaryNode
	if err := decodeSummaryNode(data, &node); err != nil {
		return SummaryNode{}, false, fmt.Errorf("%s: decode node %q/%q: %w", summaryDAGErrPrefix, conversationID, id, err)
	}
	return node, true, nil
}

func (s *SummaryWorkspaceStore) writeNode(ctx context.Context, node SummaryNode) error {
	data, err := encodeSummaryNode(node)
	if err != nil {
		return fmt.Errorf("%s: marshal node %q/%q: %w", summaryDAGErrPrefix, node.ConversationID, node.ID, err)
	}

	livePath := s.nodePath(node.ConversationID, node.ID)
	tmpPath := s.tmpNodePath(livePath)
	if err := s.ws.Write(ctx, tmpPath, data); err != nil {
		return fmt.Errorf("%s: write node tmp %q/%q: %w", summaryDAGErrPrefix, node.ConversationID, node.ID, err)
	}
	if err := s.ws.Rename(ctx, tmpPath, livePath); err != nil {
		_ = s.ws.Delete(ctx, tmpPath)
		return fmt.Errorf("%s: publish node %q/%q: %w", summaryDAGErrPrefix, node.ConversationID, node.ID, err)
	}
	return nil
}

func (s *SummaryWorkspaceStore) tmpNodePath(livePath string) string {
	return fmt.Sprintf("%s.tmp.%d.%d.%d", livePath, os.Getpid(), time.Now().UnixNano(), s.tmpCounter.Add(1))
}

func (s *SummaryWorkspaceStore) conversationDir(conversationID string) string {
	return path.Join("conversations", s.pathSegment(conversationID))
}

func (s *SummaryWorkspaceStore) nodesDir(conversationID string) string {
	return path.Join(s.conversationDir(conversationID), "nodes")
}

func (s *SummaryWorkspaceStore) nodePath(conversationID string, id NodeID) string {
	return path.Join(s.nodesDir(conversationID), s.pathSegment(string(id))+".json")
}

func (s *SummaryWorkspaceStore) pathSegment(id string) string {
	return s.pathSegmentPrefix + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func (s *SummaryWorkspaceStore) rawPathSegment(segment string) (string, error) {
	if !strings.HasPrefix(segment, s.pathSegmentPrefix) {
		return "", fmt.Errorf("missing %q prefix", s.pathSegmentPrefix)
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(segment, s.pathSegmentPrefix))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

type summaryNodeRecord struct {
	ID             NodeID              `json:"id"`
	ConversationID string              `json:"conversation_id"`
	ParentIDs      []NodeID            `json:"parent_ids,omitempty"`
	SourceRefs     []sourceRefRecord   `json:"source_refs,omitempty"`
	Summary        string              `json:"summary"`
	Level          int                 `json:"level"`
	Signature      views.ViewSignature `json:"signature"`
	CreatedAt      time.Time           `json:"created_at"`
	UpdatedAt      time.Time           `json:"updated_at"`
	Metadata       map[string]any      `json:"metadata,omitempty"`
}

type sourceRefRecord struct {
	Kind     views.SourceKind         `json:"kind"`
	Message  *views.MessageSourceRef  `json:"message,omitempty"`
	Document *views.DocumentSourceRef `json:"document,omitempty"`
}

func encodeSummaryNode(node SummaryNode) ([]byte, error) {
	return json.Marshal(summaryNodeRecord{
		ID:             node.ID,
		ConversationID: node.ConversationID,
		ParentIDs:      append([]NodeID(nil), node.ParentIDs...),
		SourceRefs:     sourceRefRecords(node.SourceRefs),
		Summary:        node.Summary,
		Level:          node.Level,
		Signature:      node.Signature,
		CreatedAt:      node.CreatedAt,
		UpdatedAt:      node.UpdatedAt,
		Metadata:       mapsClone(node.Metadata),
	})
}

func decodeSummaryNode(data []byte, node *SummaryNode) error {
	var record summaryNodeRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	*node = SummaryNode{
		ID:             record.ID,
		ConversationID: record.ConversationID,
		ParentIDs:      append([]NodeID(nil), record.ParentIDs...),
		SourceRefs:     sourceRefsFromRecords(record.SourceRefs),
		Summary:        record.Summary,
		Level:          record.Level,
		Signature:      record.Signature,
		CreatedAt:      record.CreatedAt,
		UpdatedAt:      record.UpdatedAt,
		Metadata:       mapsClone(record.Metadata),
	}
	return nil
}

func sourceRefRecords(refs []views.SourceRef) []sourceRefRecord {
	if refs == nil {
		return nil
	}
	records := make([]sourceRefRecord, len(refs))
	for i, ref := range refs {
		records[i] = sourceRefRecord{
			Kind:     ref.Kind,
			Message:  cloneMessageSourceRef(ref.Message),
			Document: cloneDocumentSourceRef(ref.Document),
		}
	}
	return records
}

func sourceRefsFromRecords(records []sourceRefRecord) []views.SourceRef {
	if records == nil {
		return nil
	}
	refs := make([]views.SourceRef, len(records))
	for i, record := range records {
		refs[i] = views.SourceRef{
			Kind:     record.Kind,
			Message:  cloneMessageSourceRef(record.Message),
			Document: cloneDocumentSourceRef(record.Document),
		}
	}
	return refs
}

func cloneMessageSourceRef(in *views.MessageSourceRef) *views.MessageSourceRef {
	if in == nil {
		return nil
	}
	out := *in
	if in.Span != nil {
		span := *in.Span
		out.Span = &span
	}
	return &out
}

func cloneDocumentSourceRef(in *views.DocumentSourceRef) *views.DocumentSourceRef {
	if in == nil {
		return nil
	}
	out := *in
	if in.Span != nil {
		span := *in.Span
		out.Span = &span
	}
	return &out
}

func mapsClone[K comparable, V any](in map[K]V) map[K]V {
	if in == nil {
		return nil
	}
	out := make(map[K]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
