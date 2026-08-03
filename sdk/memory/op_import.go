package memory

import "context"

// ImportRequest ingests a document and derives chunks (and
// embeddings, when an embedder is configured). The runtime runs
// this synchronously in v1; an async JobID is a future extension.
//
// Source is opaque: a path, URI, or any locator the implementation
// recognises. DatasetID is a soft partition for the document
// collection; Tags are opaque annotations that flow into the
// resulting chunks' metadata. ChunkPolicy controls how the source
// is split; a zero ChunkPolicy means "use the runtime default".
type ImportRequest struct {
	Scope       Scope
	DatasetID   string
	Source      string
	Tags        []string
	ChunkPolicy ChunkPolicy
}

// ImportResponse is what Import returns. DocumentID identifies the
// new document record; ChunkCount is the number of chunks the
// implementation produced.
type ImportResponse struct {
	DocumentID string
	ChunkCount int
}

// ImportOp is the contract an implementation satisfies to handle
// the Import operation.
type ImportOp interface {
	CompileImport(ctx context.Context, req ImportRequest) CompileResult
	ExecuteImport(ctx context.Context, req ImportRequest) (ImportResponse, error)
}
