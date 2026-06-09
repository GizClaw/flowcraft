package document

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
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

// WithClock sets the clock used for authoritative CreatedAt and UpdatedAt timestamps.
func WithClock(clock func() time.Time) Option {
	return func(s *WorkspaceStore) {
		if clock != nil {
			s.clock = clock
		}
	}
}

// WorkspaceStore persists documents as JSON files in a workspace.
type WorkspaceStore struct {
	ws    workspace.Workspace
	clock func() time.Time

	mu sync.RWMutex
}

var _ Store = (*WorkspaceStore)(nil)

const pathSegmentPrefix = "k_"

var tmpCounter atomic.Uint64

// NewWorkspaceStore returns a workspace-backed document Store.
func NewWorkspaceStore(ws workspace.Workspace, opts ...Option) *WorkspaceStore {
	s := &WorkspaceStore{
		ws:    ws,
		clock: time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

// Put stores a document as the authoritative value for its dataset and id.
// DatasetID and ID are required. Name is only a display name, filename, or
// legacy alias; callers migrating legacy data should set ID explicitly.
// Version and timestamps are assigned by the store, and ContentHash is
// computed from Content.
func (s *WorkspaceStore) Put(ctx context.Context, req PutRequest) (Document, error) {
	if s.ws == nil {
		return Document{}, errdefs.Validationf("document source: workspace is required")
	}

	doc := cloneDocument(req.Document)
	if doc.DatasetID == "" {
		return Document{}, errdefs.Validationf("document source: dataset_id is required")
	}
	if doc.ID == "" {
		return Document{}, errdefs.Validationf("document source: id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	prev, ok, err := s.readDocument(ctx, doc.DatasetID, doc.ID)
	if err != nil {
		return Document{}, err
	}

	now := s.clock()
	if ok {
		doc.Version = prev.Version + 1
		doc.CreatedAt = prev.CreatedAt
	} else {
		doc.Version = 1
		doc.CreatedAt = now
	}
	doc.UpdatedAt = now
	doc.ContentHash = contentHash(doc.Content)

	if err := s.writeDocument(ctx, doc); err != nil {
		return Document{}, err
	}
	return cloneDocument(doc), nil
}

// Get returns one document by dataset and document id.
func (s *WorkspaceStore) Get(ctx context.Context, datasetID, documentID string) (Document, bool, error) {
	if s.ws == nil {
		return Document{}, false, errdefs.Validationf("document source: workspace is required")
	}
	if datasetID == "" || documentID == "" {
		return Document{}, false, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	doc, ok, err := s.readDocument(ctx, datasetID, documentID)
	if err != nil {
		return Document{}, false, err
	}
	if !ok {
		return Document{}, false, nil
	}
	return cloneDocument(doc), true, nil
}

// List returns documents ordered by ascending document id.
func (s *WorkspaceStore) List(ctx context.Context, datasetID string, opts ListOptions) ([]Document, error) {
	if s.ws == nil {
		return nil, errdefs.Validationf("document source: workspace is required")
	}
	if datasetID == "" {
		return nil, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := s.ws.List(ctx, s.documentsDir(datasetID))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("document source: list dataset %q documents: %w", datasetID, err)
	}

	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		segment := strings.TrimSuffix(entry.Name(), ".json")
		if !strings.HasPrefix(segment, pathSegmentPrefix) {
			continue
		}
		id, err := rawPathSegment(segment)
		if err != nil {
			return nil, fmt.Errorf("document source: decode document id %q: %w", entry.Name(), err)
		}
		if id > opts.AfterID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	out := make([]Document, 0, len(ids))
	for _, id := range ids {
		doc, ok, err := s.readDocument(ctx, datasetID, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, cloneDocument(doc))
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out, nil
}

// Delete removes one document. It is idempotent.
func (s *WorkspaceStore) Delete(ctx context.Context, datasetID, documentID string) error {
	if s.ws == nil {
		return errdefs.Validationf("document source: workspace is required")
	}
	if datasetID == "" || documentID == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ws.Delete(ctx, s.documentPath(datasetID, documentID)); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("document source: delete document %q/%q: %w", datasetID, documentID, err)
	}
	return s.removeDatasetIfEmpty(ctx, datasetID)
}

// DeleteDataset removes a dataset and all of its documents. It is idempotent.
func (s *WorkspaceStore) DeleteDataset(ctx context.Context, datasetID string) error {
	if s.ws == nil {
		return errdefs.Validationf("document source: workspace is required")
	}
	if datasetID == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ws.RemoveAll(ctx, s.datasetDir(datasetID)); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("document source: delete dataset %q: %w", datasetID, err)
	}
	return nil
}

// ListDatasets returns dataset ids in ascending order.
func (s *WorkspaceStore) ListDatasets(ctx context.Context) ([]string, error) {
	if s.ws == nil {
		return nil, errdefs.Validationf("document source: workspace is required")
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := s.ws.List(ctx, "datasets")
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("document source: list datasets: %w", err)
	}

	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if !strings.HasPrefix(entry.Name(), pathSegmentPrefix) {
			continue
		}
		datasetID, err := rawPathSegment(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("document source: decode dataset id %q: %w", entry.Name(), err)
		}
		hasDocuments, err := s.datasetHasDocuments(ctx, datasetID)
		if err != nil {
			return nil, err
		}
		if hasDocuments {
			ids = append(ids, datasetID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *WorkspaceStore) readDocument(ctx context.Context, datasetID, documentID string) (Document, bool, error) {
	data, err := s.ws.Read(ctx, s.documentPath(datasetID, documentID))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return Document{}, false, nil
		}
		return Document{}, false, fmt.Errorf("document source: read document %q/%q: %w", datasetID, documentID, err)
	}

	var doc Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return Document{}, false, fmt.Errorf("document source: decode document %q/%q: %w", datasetID, documentID, err)
	}
	return doc, true, nil
}

func (s *WorkspaceStore) writeDocument(ctx context.Context, doc Document) error {
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("document source: marshal document %q/%q: %w", doc.DatasetID, doc.ID, err)
	}

	livePath := s.documentPath(doc.DatasetID, doc.ID)
	tmpPath := s.tmpDocumentPath(livePath)
	if err := s.ws.Write(ctx, tmpPath, data); err != nil {
		return fmt.Errorf("document source: write document tmp %q/%q: %w", doc.DatasetID, doc.ID, err)
	}
	if err := s.ws.Rename(ctx, tmpPath, livePath); err != nil {
		_ = s.ws.Delete(ctx, tmpPath)
		return fmt.Errorf("document source: publish document %q/%q: %w", doc.DatasetID, doc.ID, err)
	}
	return nil
}

func (s *WorkspaceStore) tmpDocumentPath(livePath string) string {
	// The store lock serializes writes within this instance. The unique tmp
	// suffix prevents fixed-path tmp collisions between store instances, but the
	// workspace API does not provide CAS or cross-process locks; callers that
	// concurrently write the same document through different stores/processes
	// still need external locking or a backend with stronger concurrency control.
	return fmt.Sprintf("%s.tmp.%d.%d.%d", livePath, os.Getpid(), time.Now().UnixNano(), tmpCounter.Add(1))
}

func (s *WorkspaceStore) removeDatasetIfEmpty(ctx context.Context, datasetID string) error {
	hasDocuments, err := s.datasetHasDocuments(ctx, datasetID)
	if err != nil {
		return err
	}
	if hasDocuments {
		return nil
	}
	if err := s.ws.RemoveAll(ctx, s.datasetDir(datasetID)); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("document source: remove empty dataset %q: %w", datasetID, err)
	}
	return nil
}

func (s *WorkspaceStore) datasetHasDocuments(ctx context.Context, datasetID string) (bool, error) {
	entries, err := s.ws.List(ctx, s.documentsDir(datasetID))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("document source: list dataset %q documents: %w", datasetID, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") && strings.HasPrefix(strings.TrimSuffix(entry.Name(), ".json"), pathSegmentPrefix) {
			return true, nil
		}
	}
	return false, nil
}

func (s *WorkspaceStore) datasetDir(datasetID string) string {
	return path.Join("datasets", pathSegment(datasetID))
}

func (s *WorkspaceStore) documentsDir(datasetID string) string {
	return path.Join(s.datasetDir(datasetID), "documents")
}

func (s *WorkspaceStore) documentPath(datasetID, documentID string) string {
	return path.Join(s.documentsDir(datasetID), pathSegment(documentID)+".json")
}

func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func pathSegment(id string) string {
	return pathSegmentPrefix + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func rawPathSegment(segment string) (string, error) {
	if !strings.HasPrefix(segment, pathSegmentPrefix) {
		return "", fmt.Errorf("missing %q prefix", pathSegmentPrefix)
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(segment, pathSegmentPrefix))
	if err != nil {
		return "", err
	}
	return string(data), nil
}
