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
// runtimes/{encodedRuntimeID}/users/{encodedUserID}/agents/{encodedAgentID}/conversations/{encodedConversationID}/nodes/{encodedNodeID}.json
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
var _ SummaryNodeDeleter = (*SummaryWorkspaceStore)(nil)

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

// PutNode stores a summary node as the authoritative value for its scoped
// conversation and node id.
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

// GetNode returns one summary node by scope and node id.
func (s *SummaryWorkspaceStore) GetNode(ctx context.Context, scope views.Scope, id NodeID) (SummaryNode, bool, error) {
	if s.ws == nil {
		return SummaryNode{}, false, errdefs.Validationf("%s: workspace is required", summaryDAGErrPrefix)
	}
	if scope.ConversationID == "" || id == "" {
		return SummaryNode{}, false, nil
	}
	if err := validateSummaryScope(scope); err != nil {
		return SummaryNode{}, false, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	node, ok, err := s.readNode(ctx, scope, id)
	if err != nil {
		return SummaryNode{}, false, err
	}
	if !ok {
		return SummaryNode{}, false, nil
	}
	return cloneSummaryNode(node), true, nil
}

// ListNodes returns summary nodes ordered by ascending node id.
func (s *SummaryWorkspaceStore) ListNodes(ctx context.Context, scope views.Scope, opts ListOptions) ([]SummaryNode, error) {
	if s.ws == nil {
		return nil, errdefs.Validationf("%s: workspace is required", summaryDAGErrPrefix)
	}
	if scope.ConversationID == "" {
		return nil, nil
	}
	if err := validateSummaryScope(scope); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := s.ws.List(ctx, s.nodesDir(scope))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: list scope %q/%q/%q/%q nodes: %w", summaryDAGErrPrefix, scope.RuntimeID, scope.UserID, scope.AgentID, scope.ConversationID, err)
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
		node, ok, err := s.readNode(ctx, scope, NodeID(id))
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

// DeleteNode removes one persisted summary node for a scoped conversation. It is
// path-based and does not decode the node payload, so stale or incompatible node
// contents do not block targeted cleanup.
func (s *SummaryWorkspaceStore) DeleteNode(ctx context.Context, scope views.Scope, id NodeID) error {
	if s.ws == nil {
		return errdefs.Validationf("%s: workspace is required", summaryDAGErrPrefix)
	}
	if scope.ConversationID == "" || id == "" {
		return nil
	}
	if err := validateSummaryScope(scope); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ws.Delete(ctx, s.nodePath(scope, id)); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("%s: delete node %q/%q/%q/%q/%q: %w", summaryDAGErrPrefix, scope.RuntimeID, scope.UserID, scope.AgentID, scope.ConversationID, id, err)
	}
	return nil
}

// DeleteScope removes all persisted summary nodes for a scoped conversation. It
// is idempotent.
func (s *SummaryWorkspaceStore) DeleteScope(ctx context.Context, scope views.Scope) error {
	if s.ws == nil {
		return errdefs.Validationf("%s: workspace is required", summaryDAGErrPrefix)
	}
	if scope.ConversationID == "" {
		return nil
	}
	if err := validateSummaryScope(scope); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ws.RemoveAll(ctx, s.conversationDir(scope)); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("%s: delete scope %q/%q/%q/%q: %w", summaryDAGErrPrefix, scope.RuntimeID, scope.UserID, scope.AgentID, scope.ConversationID, err)
	}
	return nil
}

func validateSummaryScope(scope views.Scope) error {
	if err := scope.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid scope: %w", summaryDAGErrPrefix, err)
	}
	return nil
}

func (s *SummaryWorkspaceStore) readNode(ctx context.Context, scope views.Scope, id NodeID) (SummaryNode, bool, error) {
	data, err := s.ws.Read(ctx, s.nodePath(scope, id))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return SummaryNode{}, false, nil
		}
		return SummaryNode{}, false, fmt.Errorf("%s: read node %q/%q/%q/%q/%q: %w", summaryDAGErrPrefix, scope.RuntimeID, scope.UserID, scope.AgentID, scope.ConversationID, id, err)
	}

	var node SummaryNode
	if err := decodeSummaryNode(data, &node); err != nil {
		return SummaryNode{}, false, fmt.Errorf("%s: decode node %q/%q/%q/%q/%q: %w", summaryDAGErrPrefix, scope.RuntimeID, scope.UserID, scope.AgentID, scope.ConversationID, id, err)
	}
	if !sameSummaryNodeScope(node.Scope, scope) {
		return SummaryNode{}, false, nil
	}
	return node, true, nil
}

func (s *SummaryWorkspaceStore) writeNode(ctx context.Context, node SummaryNode) error {
	data, err := encodeSummaryNode(node)
	if err != nil {
		return fmt.Errorf("%s: marshal node %q/%q: %w", summaryDAGErrPrefix, node.Scope.ConversationID, node.ID, err)
	}

	livePath := s.nodePath(node.Scope, node.ID)
	tmpPath := s.tmpNodePath(livePath)
	if err := s.ws.Write(ctx, tmpPath, data); err != nil {
		return fmt.Errorf("%s: write node tmp %q/%q: %w", summaryDAGErrPrefix, node.Scope.ConversationID, node.ID, err)
	}
	if err := s.ws.Rename(ctx, tmpPath, livePath); err != nil {
		_ = s.ws.Delete(ctx, tmpPath)
		return fmt.Errorf("%s: publish node %q/%q: %w", summaryDAGErrPrefix, node.Scope.ConversationID, node.ID, err)
	}
	return nil
}

func (s *SummaryWorkspaceStore) tmpNodePath(livePath string) string {
	return fmt.Sprintf("%s.tmp.%d.%d.%d", livePath, os.Getpid(), time.Now().UnixNano(), s.tmpCounter.Add(1))
}

func (s *SummaryWorkspaceStore) runtimeDir(scope views.Scope) string {
	return path.Join("runtimes", s.pathSegment(scope.RuntimeID))
}

func (s *SummaryWorkspaceStore) userDir(scope views.Scope) string {
	return path.Join(s.runtimeDir(scope), "users", s.pathSegment(scope.UserID))
}

func (s *SummaryWorkspaceStore) agentDir(scope views.Scope) string {
	return path.Join(s.userDir(scope), "agents", s.pathSegment(scope.AgentID))
}

func (s *SummaryWorkspaceStore) conversationDir(scope views.Scope) string {
	return path.Join(s.agentDir(scope), "conversations", s.pathSegment(scope.ConversationID))
}

func (s *SummaryWorkspaceStore) nodesDir(scope views.Scope) string {
	return path.Join(s.conversationDir(scope), "nodes")
}

func (s *SummaryWorkspaceStore) nodePath(scope views.Scope, id NodeID) string {
	return path.Join(s.nodesDir(scope), s.pathSegment(string(id))+".json")
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

func sameSummaryNodeScope(left, right views.Scope) bool {
	return left.RuntimeID == right.RuntimeID &&
		left.UserID == right.UserID &&
		left.AgentID == right.AgentID &&
		left.ConversationID == right.ConversationID
}

type summaryNodeRecord struct {
	ID         NodeID              `json:"id"`
	Scope      views.Scope         `json:"scope"`
	ParentIDs  []NodeID            `json:"parent_ids,omitempty"`
	SourceRefs []sourceRefRecord   `json:"source_refs,omitempty"`
	Summary    string              `json:"summary"`
	Level      int                 `json:"level"`
	Signature  views.ViewSignature `json:"signature"`
	CreatedAt  time.Time           `json:"created_at"`
	UpdatedAt  time.Time           `json:"updated_at"`
	Metadata   map[string]any      `json:"metadata,omitempty"`
}

type sourceRefRecord struct {
	Kind     views.SourceKind         `json:"kind"`
	Message  *views.MessageSourceRef  `json:"message,omitempty"`
	Document *views.DocumentSourceRef `json:"document,omitempty"`
}

func encodeSummaryNode(node SummaryNode) ([]byte, error) {
	return json.Marshal(summaryNodeRecord{
		ID:         node.ID,
		Scope:      node.Scope,
		ParentIDs:  append([]NodeID(nil), node.ParentIDs...),
		SourceRefs: sourceRefRecords(node.SourceRefs),
		Summary:    node.Summary,
		Level:      node.Level,
		Signature:  node.Signature,
		CreatedAt:  node.CreatedAt,
		UpdatedAt:  node.UpdatedAt,
		Metadata:   mapsClone(node.Metadata),
	})
}

func decodeSummaryNode(data []byte, node *SummaryNode) error {
	var record summaryNodeRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	*node = SummaryNode{
		ID:         record.ID,
		Scope:      record.Scope,
		ParentIDs:  append([]NodeID(nil), record.ParentIDs...),
		SourceRefs: sourceRefsFromRecords(record.SourceRefs),
		Summary:    record.Summary,
		Level:      record.Level,
		Signature:  record.Signature,
		CreatedAt:  record.CreatedAt,
		UpdatedAt:  record.UpdatedAt,
		Metadata:   mapsClone(record.Metadata),
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
