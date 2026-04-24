package knowledge

import "context"

// DocInput is a name+content pair for batch document ingestion.
//
// Deprecated: use Service.PutDocument (one call per document).
// Removed in v0.3.0.
type DocInput struct {
	Name    string
	Content string
}

// Store abstracts knowledge base storage. Documents are organized by dataset.
//
// Deprecated: use *Service in sdk/knowledge instead. Service unifies
// document, chunk and layer storage behind a single contract and is the
// only orchestrator going forward; Store will be removed in v0.3.0.
//
// Migration:
//   - Replace AddDocument / AddDocuments with Service.PutDocument.
//   - Replace Search                          with Service.Search.
//   - Replace Abstract / Overview             with Service.Layer.
//   - Replace DatasetAbstract / Overview      with Service.DatasetLayer.
type Store interface {
	AddDocument(ctx context.Context, datasetID, name, content string) error
	AddDocuments(ctx context.Context, datasetID string, docs []DocInput) error
	GetDocument(ctx context.Context, datasetID, name string) (*Document, error)
	DeleteDocument(ctx context.Context, datasetID, name string) error
	ListDocuments(ctx context.Context, datasetID string) ([]Document, error)
	Search(ctx context.Context, datasetID, query string, opts SearchOptions) ([]SearchResult, error)

	// Layered reads
	Abstract(ctx context.Context, datasetID, name string) (string, error)
	Overview(ctx context.Context, datasetID, name string) (string, error)

	// Dataset-level summaries
	DatasetAbstract(ctx context.Context, datasetID string) (string, error)
	DatasetOverview(ctx context.Context, datasetID string) (string, error)
}
