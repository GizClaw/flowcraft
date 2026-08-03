package memory

import "context"

// RecallRequest is a pure retrieval call. It does not mutate state
// and never invokes an LLM. The runtime compiles each field and
// refuses requests whose ledger is incomplete.
//
// Query is the only required field. TopK bounds the result count;
// MinScore filters low-relevance hits; Filters carries opaque
// key/value hints (e.g. dataset=knowledge_base) that implementations
// apply during scoring.
type RecallRequest struct {
	Scope          Scope
	ConversationID string
	Query          string
	TopK           int
	Filters        map[string]string
	MinScore       float64
}

// RecallResponse is what Recall returns. Hits is sorted by Score
// descending; implementations guarantee this so callers can
// truncate by len(Hits) without re-sorting.
type RecallResponse struct {
	Hits []Hit
}

// RecallOp is the contract an implementation satisfies to handle
// the Recall operation.
type RecallOp interface {
	CompileRecall(ctx context.Context, req RecallRequest) CompileResult
	ExecuteRecall(ctx context.Context, req RecallRequest) (RecallResponse, error)
}
