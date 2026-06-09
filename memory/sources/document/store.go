package document

import "context"

// PutRequest stores or replaces one canonical document.
type PutRequest struct {
	Document Document
}

// ListOptions controls ordered document scans within a dataset.
type ListOptions struct {
	Limit   int
	AfterID string
}

// Store persists canonical external documents.
type Store interface {
	Put(ctx context.Context, req PutRequest) (Document, error)
	Get(ctx context.Context, datasetID, documentID string) (Document, bool, error)
	List(ctx context.Context, datasetID string, opts ListOptions) ([]Document, error)
	Delete(ctx context.Context, datasetID, documentID string) error
	DeleteDataset(ctx context.Context, datasetID string) error
	ListDatasets(ctx context.Context) ([]string, error)
}
