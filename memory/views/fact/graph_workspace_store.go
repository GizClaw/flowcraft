package fact

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

// GraphWorkspaceStore persists fact graph nodes and edges as JSON files in a workspace.
//
// Nodes and edges are stored under:
// graph/nodes/{encodedNodeID}.json
// graph/edges/{encodedEdgeID}.json
//
// Concurrent writes to the same scoped workspace must go through one
// GraphWorkspaceStore instance. Cross-instance or cross-process writers require
// an external lock or a workspace backend with stronger concurrency guarantees.
type GraphWorkspaceStore struct {
	ws                workspace.Workspace
	pathSegmentPrefix string
	tmpCounter        atomic.Uint64

	mu sync.RWMutex
}

var _ GraphStore = (*GraphWorkspaceStore)(nil)

// defaultGraphPathSegmentPrefix marks encoded workspace path segments. It is
// not part of node IDs, edge IDs, or other business identifiers.
const defaultGraphPathSegmentPrefix = "fgraph_"

// GraphWorkspaceStoreOption configures a GraphWorkspaceStore.
type GraphWorkspaceStoreOption interface {
	applyGraphWorkspaceStore(*GraphWorkspaceStore)
}

type graphPathSegmentPrefixOption string

// WithGraphPathSegmentPrefix sets the encoded workspace path segment marker.
// Passing an empty prefix is explicit and disables the marker.
func WithGraphPathSegmentPrefix(prefix string) GraphWorkspaceStoreOption {
	return graphPathSegmentPrefixOption(prefix)
}

func (o graphPathSegmentPrefixOption) applyGraphWorkspaceStore(s *GraphWorkspaceStore) {
	s.pathSegmentPrefix = string(o)
}

// NewGraphWorkspaceStore returns a workspace-backed fact graph store.
func NewGraphWorkspaceStore(ws workspace.Workspace, opts ...GraphWorkspaceStoreOption) *GraphWorkspaceStore {
	s := &GraphWorkspaceStore{
		ws:                ws,
		pathSegmentPrefix: defaultGraphPathSegmentPrefix,
	}
	for _, opt := range opts {
		if opt != nil {
			opt.applyGraphWorkspaceStore(s)
		}
	}
	return s
}

// PutNode stores or replaces the authoritative graph node for its id. Empty
// kind is normalized to entity.
func (s *GraphWorkspaceStore) PutNode(ctx context.Context, node Node) (Node, error) {
	if s.ws == nil {
		return Node{}, errdefs.Validationf("%s: workspace is required", graphErrPrefix)
	}
	node = normalizeNode(cloneNode(node))
	if err := validateNode(node); err != nil {
		return Node{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writeNode(ctx, node); err != nil {
		return Node{}, err
	}
	return cloneNode(node), nil
}

// GetNode returns one graph node by id.
func (s *GraphWorkspaceStore) GetNode(ctx context.Context, id NodeID) (Node, bool, error) {
	if s.ws == nil {
		return Node{}, false, errdefs.Validationf("%s: workspace is required", graphErrPrefix)
	}
	if err := validateNodeID(id); err != nil {
		return Node{}, false, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	node, ok, err := s.readNode(ctx, id)
	if err != nil {
		return Node{}, false, err
	}
	if !ok {
		return Node{}, false, nil
	}
	return cloneNode(node), true, nil
}

// ListNodes returns nodes ordered by ascending node id.
func (s *GraphWorkspaceStore) ListNodes(ctx context.Context, opts NodeListOptions) ([]Node, error) {
	if s.ws == nil {
		return nil, errdefs.Validationf("%s: workspace is required", graphErrPrefix)
	}
	opts = normalizeNodeListOptions(cloneNodeListOptions(opts))
	if err := validateNodeListOptions(opts); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	ids, err := s.nodeIDs(ctx, opts.AfterID)
	if err != nil {
		return nil, err
	}

	out := make([]Node, 0, len(ids))
	for _, id := range ids {
		node, ok, err := s.readNode(ctx, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if opts.Kind != nil && node.Kind != *opts.Kind {
			continue
		}
		if opts.Label != "" && node.Label != opts.Label {
			continue
		}
		out = append(out, cloneNode(node))
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out, nil
}

// DeleteNode removes a node and any edges that reference it. It is idempotent.
func (s *GraphWorkspaceStore) DeleteNode(ctx context.Context, id NodeID) error {
	if s.ws == nil {
		return errdefs.Validationf("%s: workspace is required", graphErrPrefix)
	}
	if err := validateNodeID(id); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ws.Delete(ctx, s.nodePath(id)); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("%s: delete node %q: %w", graphErrPrefix, id, err)
	}

	edgeIDs, err := s.edgeIDs(ctx, "")
	if err != nil {
		return err
	}
	for _, edgeID := range edgeIDs {
		edge, ok, err := s.readEdge(ctx, edgeID)
		if err != nil {
			return err
		}
		if !ok || (edge.From != id && edge.To != id) {
			continue
		}
		if err := s.ws.Delete(ctx, s.edgePath(edgeID)); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("%s: delete edge %q for node %q: %w", graphErrPrefix, edgeID, id, err)
		}
	}
	return nil
}

// PutEdge stores or replaces the authoritative graph edge for its id. Empty
// status is normalized to active.
func (s *GraphWorkspaceStore) PutEdge(ctx context.Context, edge Edge) (Edge, error) {
	if s.ws == nil {
		return Edge{}, errdefs.Validationf("%s: workspace is required", graphErrPrefix)
	}
	edge = normalizeEdge(cloneEdge(edge))
	if err := validateEdge(edge); err != nil {
		return Edge{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writeEdge(ctx, edge); err != nil {
		return Edge{}, err
	}
	return cloneEdge(edge), nil
}

// GetEdge returns one graph edge by id.
func (s *GraphWorkspaceStore) GetEdge(ctx context.Context, id EdgeID) (Edge, bool, error) {
	if s.ws == nil {
		return Edge{}, false, errdefs.Validationf("%s: workspace is required", graphErrPrefix)
	}
	if err := validateEdgeID(id); err != nil {
		return Edge{}, false, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	edge, ok, err := s.readEdge(ctx, id)
	if err != nil {
		return Edge{}, false, err
	}
	if !ok {
		return Edge{}, false, nil
	}
	return cloneEdge(edge), true, nil
}

// ListEdges returns edges ordered by ascending edge id.
func (s *GraphWorkspaceStore) ListEdges(ctx context.Context, opts EdgeListOptions) ([]Edge, error) {
	if s.ws == nil {
		return nil, errdefs.Validationf("%s: workspace is required", graphErrPrefix)
	}
	opts = normalizeEdgeListOptions(cloneEdgeListOptions(opts))
	if err := validateEdgeListOptions(opts); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	ids, err := s.edgeIDs(ctx, opts.AfterID)
	if err != nil {
		return nil, err
	}

	out := make([]Edge, 0, len(ids))
	for _, id := range ids {
		edge, ok, err := s.readEdge(ctx, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if opts.From != "" && edge.From != opts.From {
			continue
		}
		if opts.To != "" && edge.To != opts.To {
			continue
		}
		if opts.Predicate != "" && edge.Predicate != opts.Predicate {
			continue
		}
		if opts.Status != nil && edge.Status != *opts.Status {
			continue
		}
		out = append(out, cloneEdge(edge))
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out, nil
}

// DeleteEdge removes one edge by id. It is idempotent.
func (s *GraphWorkspaceStore) DeleteEdge(ctx context.Context, id EdgeID) error {
	if s.ws == nil {
		return errdefs.Validationf("%s: workspace is required", graphErrPrefix)
	}
	if err := validateEdgeID(id); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ws.Delete(ctx, s.edgePath(id)); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("%s: delete edge %q: %w", graphErrPrefix, id, err)
	}
	return nil
}

func (s *GraphWorkspaceStore) nodeIDs(ctx context.Context, afterID NodeID) ([]NodeID, error) {
	entries, err := s.ws.List(ctx, s.nodesDir())
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: list nodes: %w", graphErrPrefix, err)
	}

	ids := make([]NodeID, 0, len(entries))
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
			return nil, fmt.Errorf("%s: decode node id %q: %w", graphErrPrefix, entry.Name(), err)
		}
		nodeID := NodeID(id)
		if nodeID > afterID {
			ids = append(ids, nodeID)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		return ids[i] < ids[j]
	})
	return ids, nil
}

func (s *GraphWorkspaceStore) edgeIDs(ctx context.Context, afterID EdgeID) ([]EdgeID, error) {
	entries, err := s.ws.List(ctx, s.edgesDir())
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: list edges: %w", graphErrPrefix, err)
	}

	ids := make([]EdgeID, 0, len(entries))
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
			return nil, fmt.Errorf("%s: decode edge id %q: %w", graphErrPrefix, entry.Name(), err)
		}
		edgeID := EdgeID(id)
		if edgeID > afterID {
			ids = append(ids, edgeID)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		return ids[i] < ids[j]
	})
	return ids, nil
}

func (s *GraphWorkspaceStore) readNode(ctx context.Context, id NodeID) (Node, bool, error) {
	data, err := s.ws.Read(ctx, s.nodePath(id))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return Node{}, false, nil
		}
		return Node{}, false, fmt.Errorf("%s: read node %q: %w", graphErrPrefix, id, err)
	}

	var node Node
	if err := decodeNode(data, &node); err != nil {
		return Node{}, false, fmt.Errorf("%s: decode node %q: %w", graphErrPrefix, id, err)
	}
	return normalizeNode(node), true, nil
}

func (s *GraphWorkspaceStore) readEdge(ctx context.Context, id EdgeID) (Edge, bool, error) {
	data, err := s.ws.Read(ctx, s.edgePath(id))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return Edge{}, false, nil
		}
		return Edge{}, false, fmt.Errorf("%s: read edge %q: %w", graphErrPrefix, id, err)
	}

	var edge Edge
	if err := decodeEdge(data, &edge); err != nil {
		return Edge{}, false, fmt.Errorf("%s: decode edge %q: %w", graphErrPrefix, id, err)
	}
	return normalizeEdge(edge), true, nil
}

func (s *GraphWorkspaceStore) writeNode(ctx context.Context, node Node) error {
	data, err := encodeNode(node)
	if err != nil {
		return fmt.Errorf("%s: marshal node %q: %w", graphErrPrefix, node.ID, err)
	}

	livePath := s.nodePath(node.ID)
	tmpPath := s.tmpGraphPath(livePath)
	if err := s.ws.Write(ctx, tmpPath, data); err != nil {
		return fmt.Errorf("%s: write node tmp %q: %w", graphErrPrefix, node.ID, err)
	}
	if err := s.ws.Rename(ctx, tmpPath, livePath); err != nil {
		_ = s.ws.Delete(ctx, tmpPath)
		return fmt.Errorf("%s: publish node %q: %w", graphErrPrefix, node.ID, err)
	}
	return nil
}

func (s *GraphWorkspaceStore) writeEdge(ctx context.Context, edge Edge) error {
	data, err := encodeEdge(edge)
	if err != nil {
		return fmt.Errorf("%s: marshal edge %q: %w", graphErrPrefix, edge.ID, err)
	}

	livePath := s.edgePath(edge.ID)
	tmpPath := s.tmpGraphPath(livePath)
	if err := s.ws.Write(ctx, tmpPath, data); err != nil {
		return fmt.Errorf("%s: write edge tmp %q: %w", graphErrPrefix, edge.ID, err)
	}
	if err := s.ws.Rename(ctx, tmpPath, livePath); err != nil {
		_ = s.ws.Delete(ctx, tmpPath)
		return fmt.Errorf("%s: publish edge %q: %w", graphErrPrefix, edge.ID, err)
	}
	return nil
}

func (s *GraphWorkspaceStore) tmpGraphPath(livePath string) string {
	return fmt.Sprintf("%s.tmp.%d.%d.%d", livePath, os.Getpid(), time.Now().UnixNano(), s.tmpCounter.Add(1))
}

func (s *GraphWorkspaceStore) nodesDir() string {
	return path.Join("graph", "nodes")
}

func (s *GraphWorkspaceStore) edgesDir() string {
	return path.Join("graph", "edges")
}

func (s *GraphWorkspaceStore) nodePath(id NodeID) string {
	return path.Join(s.nodesDir(), s.pathSegment(string(id))+".json")
}

func (s *GraphWorkspaceStore) edgePath(id EdgeID) string {
	return path.Join(s.edgesDir(), s.pathSegment(string(id))+".json")
}

func (s *GraphWorkspaceStore) pathSegment(id string) string {
	return s.pathSegmentPrefix + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func (s *GraphWorkspaceStore) rawPathSegment(segment string) (string, error) {
	if !strings.HasPrefix(segment, s.pathSegmentPrefix) {
		return "", fmt.Errorf("missing %q prefix", s.pathSegmentPrefix)
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(segment, s.pathSegmentPrefix))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

type nodeRecord struct {
	ID         NodeID              `json:"id"`
	Scope      views.Scope         `json:"scope"`
	Kind       NodeKind            `json:"kind"`
	Label      string              `json:"label"`
	Aliases    []string            `json:"aliases,omitempty"`
	FactRefs   []factRefRecord     `json:"fact_refs,omitempty"`
	SourceRefs []sourceRefRecord   `json:"source_refs,omitempty"`
	Signature  views.ViewSignature `json:"signature"`
	CreatedAt  time.Time           `json:"created_at"`
	UpdatedAt  time.Time           `json:"updated_at"`
	Metadata   map[string]any      `json:"metadata,omitempty"`
}

type edgeRecord struct {
	ID         EdgeID              `json:"id"`
	Scope      views.Scope         `json:"scope"`
	From       NodeID              `json:"from"`
	To         NodeID              `json:"to"`
	Predicate  string              `json:"predicate"`
	Status     FactStatus          `json:"status"`
	Confidence float64             `json:"confidence"`
	ValidFrom  *time.Time          `json:"valid_from,omitempty"`
	ValidUntil *time.Time          `json:"valid_until,omitempty"`
	FactRefs   []factRefRecord     `json:"fact_refs,omitempty"`
	SourceRefs []sourceRefRecord   `json:"source_refs,omitempty"`
	Signature  views.ViewSignature `json:"signature"`
	CreatedAt  time.Time           `json:"created_at"`
	UpdatedAt  time.Time           `json:"updated_at"`
	Metadata   map[string]any      `json:"metadata,omitempty"`
}

type factRefRecord struct {
	FactID FactID `json:"fact_id"`
	Role   string `json:"role,omitempty"`
}

func encodeNode(node Node) ([]byte, error) {
	node = cloneNode(node)
	return json.Marshal(nodeRecord{
		ID:         node.ID,
		Scope:      node.Scope,
		Kind:       node.Kind,
		Label:      node.Label,
		Aliases:    cloneStrings(node.Aliases),
		FactRefs:   factRefRecords(node.FactRefs),
		SourceRefs: sourceRefRecords(node.SourceRefs),
		Signature:  cloneViewSignature(node.Signature),
		CreatedAt:  node.CreatedAt,
		UpdatedAt:  node.UpdatedAt,
		Metadata:   cloneAnyMap(node.Metadata),
	})
}

func decodeNode(data []byte, node *Node) error {
	var record nodeRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	*node = Node{
		ID:         record.ID,
		Scope:      record.Scope,
		Kind:       record.Kind,
		Label:      record.Label,
		Aliases:    cloneStrings(record.Aliases),
		FactRefs:   factRefsFromRecords(record.FactRefs),
		SourceRefs: sourceRefsFromRecords(record.SourceRefs),
		Signature:  cloneViewSignature(record.Signature),
		CreatedAt:  record.CreatedAt,
		UpdatedAt:  record.UpdatedAt,
		Metadata:   cloneAnyMap(record.Metadata),
	}
	return nil
}

func encodeEdge(edge Edge) ([]byte, error) {
	edge = cloneEdge(edge)
	return json.Marshal(edgeRecord{
		ID:         edge.ID,
		Scope:      edge.Scope,
		From:       edge.From,
		To:         edge.To,
		Predicate:  edge.Predicate,
		Status:     edge.Status,
		Confidence: edge.Confidence,
		ValidFrom:  cloneTimePtr(edge.ValidFrom),
		ValidUntil: cloneTimePtr(edge.ValidUntil),
		FactRefs:   factRefRecords(edge.FactRefs),
		SourceRefs: sourceRefRecords(edge.SourceRefs),
		Signature:  cloneViewSignature(edge.Signature),
		CreatedAt:  edge.CreatedAt,
		UpdatedAt:  edge.UpdatedAt,
		Metadata:   cloneAnyMap(edge.Metadata),
	})
}

func decodeEdge(data []byte, edge *Edge) error {
	var record edgeRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	*edge = Edge{
		ID:         record.ID,
		Scope:      record.Scope,
		From:       record.From,
		To:         record.To,
		Predicate:  record.Predicate,
		Status:     record.Status,
		Confidence: record.Confidence,
		ValidFrom:  cloneTimePtr(record.ValidFrom),
		ValidUntil: cloneTimePtr(record.ValidUntil),
		FactRefs:   factRefsFromRecords(record.FactRefs),
		SourceRefs: sourceRefsFromRecords(record.SourceRefs),
		Signature:  cloneViewSignature(record.Signature),
		CreatedAt:  record.CreatedAt,
		UpdatedAt:  record.UpdatedAt,
		Metadata:   cloneAnyMap(record.Metadata),
	}
	return nil
}

func factRefRecords(refs []FactRef) []factRefRecord {
	if refs == nil {
		return nil
	}
	records := make([]factRefRecord, len(refs))
	for i, ref := range refs {
		records[i] = factRefRecord{
			FactID: ref.FactID,
			Role:   ref.Role,
		}
	}
	return records
}

func factRefsFromRecords(records []factRefRecord) []FactRef {
	if records == nil {
		return nil
	}
	refs := make([]FactRef, len(records))
	for i, record := range records {
		refs[i] = FactRef{
			FactID: record.FactID,
			Role:   record.Role,
		}
	}
	return refs
}
