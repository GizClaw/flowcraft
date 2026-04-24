package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// FSDocumentRepo persists SourceDocuments under <prefix>/<dataset>/.
//
// Concurrency: Put against the same (datasetID, name) is serialised by a
// per-document mutex so the read-modify-write Version increment is
// deterministic. Reads do not acquire the per-document mutex.
type FSDocumentRepo struct {
	ws     workspace.Workspace
	paths  pathBuilder
	now    func() time.Time

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewDocumentRepo wires an FSDocumentRepo to ws under the given prefix
// ("knowledge" if empty).
func NewDocumentRepo(ws workspace.Workspace, prefix string) *FSDocumentRepo {
	return &FSDocumentRepo{
		ws:    ws,
		paths: newPathBuilder(prefix),
		now:   time.Now,
		locks: make(map[string]*sync.Mutex),
	}
}

// WithNow overrides the time source (test injection point).
func (r *FSDocumentRepo) WithNow(now func() time.Time) *FSDocumentRepo {
	if now != nil {
		r.now = now
	}
	return r
}

// docKey is the per-document mutex key.
func docKey(datasetID, name string) string { return datasetID + "\x00" + name }

func (r *FSDocumentRepo) lockFor(datasetID, name string) *sync.Mutex {
	key := docKey(datasetID, name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.locks[key]; ok {
		return m
	}
	m := &sync.Mutex{}
	r.locks[key] = m
	return m
}

// metaSidecar mirrors what we serialise alongside the raw document.
type metaSidecar struct {
	Version   uint64            `json:"version"`
	UpdatedAt time.Time         `json:"updated_at"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// Put atomically increments SourceDocument.Version, writes the raw
// content, and updates the .meta.json sidecar. Doc.Version supplied by
// the caller is ignored; the repo is the source of truth for versions.
func (r *FSDocumentRepo) Put(ctx context.Context, doc knowledge.SourceDocument) error {
	if doc.DatasetID == "" || doc.Name == "" {
		return errdefs.Validationf("knowledge/fs: dataset_id and name are required")
	}
	mu := r.lockFor(doc.DatasetID, doc.Name)
	mu.Lock()
	defer mu.Unlock()

	var prevVersion uint64
	if existing, _ := r.readSidecar(ctx, doc.DatasetID, doc.Name); existing != nil {
		prevVersion = existing.Version
	}

	nextVersion := prevVersion + 1
	now := r.now().UTC()

	if err := atomicWrite(ctx, r.ws, r.paths.documentPath(doc.DatasetID, doc.Name), []byte(doc.Content)); err != nil {
		return err
	}

	side := metaSidecar{
		Version:   nextVersion,
		UpdatedAt: now,
		Metadata:  copyMetadata(doc.Metadata),
	}
	payload, err := json.Marshal(side)
	if err != nil {
		return fmt.Errorf("knowledge/fs: marshal meta: %w", err)
	}
	if err := atomicWrite(ctx, r.ws, r.paths.metaPath(doc.DatasetID, doc.Name), payload); err != nil {
		return err
	}
	return nil
}

// Get returns the SourceDocument with Content losslessly preserved.
//
// Returns errdefs.NotFound when the document does not exist.
func (r *FSDocumentRepo) Get(ctx context.Context, datasetID, name string) (*knowledge.SourceDocument, error) {
	if datasetID == "" || name == "" {
		return nil, errdefs.Validationf("knowledge/fs: dataset_id and name are required")
	}
	docPath := r.paths.documentPath(datasetID, name)
	data, err := r.ws.Read(ctx, docPath)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, errdefs.NotFoundf("knowledge/fs: document %s/%s", datasetID, name)
		}
		return nil, fmt.Errorf("knowledge/fs: read %s/%s: %w", datasetID, name, err)
	}
	doc := &knowledge.SourceDocument{
		DatasetID: datasetID,
		Name:      name,
		Content:   string(data),
	}
	if side, _ := r.readSidecar(ctx, datasetID, name); side != nil {
		doc.Version = side.Version
		doc.UpdatedAt = side.UpdatedAt
		doc.Metadata = copyMetadata(side.Metadata)
	}
	return doc, nil
}

// Delete removes the raw document, its meta sidecar and its layer
// sidecars. Idempotent: missing files are ignored.
func (r *FSDocumentRepo) Delete(ctx context.Context, datasetID, name string) error {
	if datasetID == "" || name == "" {
		return errdefs.Validationf("knowledge/fs: dataset_id and name are required")
	}
	mu := r.lockFor(datasetID, name)
	mu.Lock()
	defer mu.Unlock()
	for _, p := range []string{
		r.paths.documentPath(datasetID, name),
		r.paths.metaPath(datasetID, name),
		r.paths.layerPath(datasetID, name, "L0"),
		r.paths.layerPath(datasetID, name, "L1"),
		r.paths.docLayersPath(datasetID, name),
	} {
		if p == "" {
			continue
		}
		if err := r.ws.Delete(ctx, p); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("knowledge/fs: delete %s: %w", p, err)
		}
	}
	return nil
}

// List returns SourceDocuments for the dataset, sorted by Name. Content
// is loaded for each document; callers that only need names should use
// the lighter ListDatasets+manual List pattern in higher tiers.
func (r *FSDocumentRepo) List(ctx context.Context, datasetID string) ([]knowledge.SourceDocument, error) {
	if datasetID == "" {
		return nil, errdefs.Validationf("knowledge/fs: dataset_id is required")
	}
	entries, err := r.ws.List(ctx, r.paths.datasetDir(datasetID))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("knowledge/fs: list %s: %w", datasetID, err)
	}
	var docs []knowledge.SourceDocument
	for _, e := range entries {
		if e.IsDir() || !isDocument(e.Name()) {
			continue
		}
		doc, err := r.Get(ctx, datasetID, e.Name())
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		docs = append(docs, *doc)
	}
	sort.SliceStable(docs, func(i, j int) bool { return docs[i].Name < docs[j].Name })
	return docs, nil
}

// ListDatasets enumerates dataset IDs by listing the prefix directory.
//
// Workspace.List in some implementations (notably MemWorkspace) emits
// the queried directory itself as an entry; we filter that case out so
// the dataset list never contains the prefix name itself.
func (r *FSDocumentRepo) ListDatasets(ctx context.Context) ([]string, error) {
	entries, err := r.ws.List(ctx, r.paths.rootDir())
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("knowledge/fs: list root: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() || r.paths.isPrefixSelfEntry(e.Name()) {
			continue
		}
		ids = append(ids, e.Name())
	}
	sort.Strings(ids)
	return ids, nil
}

// readSidecar loads the .meta.json sidecar; returns (nil, nil) when absent.
func (r *FSDocumentRepo) readSidecar(ctx context.Context, datasetID, name string) (*metaSidecar, error) {
	data, err := r.ws.Read(ctx, r.paths.metaPath(datasetID, name))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var side metaSidecar
	if err := json.Unmarshal(data, &side); err != nil {
		return nil, fmt.Errorf("knowledge/fs: parse meta %s/%s: %w", datasetID, name, err)
	}
	return &side, nil
}

// copyMetadata returns a deep copy of m, or nil when m is nil/empty.
func copyMetadata(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Compile-time interface assertion.
var _ knowledge.DocumentRepo = (*FSDocumentRepo)(nil)
