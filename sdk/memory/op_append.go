package memory

import "context"

// AppendRequest writes records to a transcript under the given
// Scope. The runtime assigns a monotonically increasing Seq per
// (HardPartitionKey, ConversationID) and returns the high-water
// mark in AppendResponse.LastSeq.
//
// Records is required. IdempotencyKey is the contract for
// retry-safe writes: a Committer that re-runs after an
// ambiguous transport failure passes the same key (the canonical
// value is agent.Identity.RunID) and the runtime dedups the
// second write against the first. ConversationID and Metadata
// are optional; Metadata is opaque key/value annotations that
// the implementation may persist alongside the records.
type AppendRequest struct {
	Scope          Scope
	ConversationID string
	// IdempotencyKey scopes dedup to one logical append.
	// Empty disables dedup; the runtime will accept the
	// request as fresh and may create duplicates on retry.
	// Committers MUST pass Identity.RunID here.
	IdempotencyKey string
	Records        []Record
	Metadata       map[string]string
}

// AppendResponse is what Append returns. Appended counts the
// records actually persisted (excluding duplicates dropped by
// IdempotencyKey). LastSeq is the highest sequence number the
// runtime assigned, useful for callers that want to checkpoint
// or resume.
type AppendResponse struct {
	Appended int
	LastSeq  uint64
}

// AppendOp is the contract an implementation satisfies to handle
// the Append operation. The interface is intentionally narrow: the
// runtime wraps the call with Compile enforcement, telemetry, and
// error translation. Implementations should not perform their own
// policy decisions; reject via Decision at the Compile stage
// instead.
type AppendOp interface {
	// CompileAppend reports how the implementation will handle
	// each canonical field of the request. It must NOT perform
	// I/O. The runtime calls it before ExecuteAppend and refuses
	// requests whose ledger is incomplete or contains a
	// Rejected field. An empty Record.ID is valid during Compile:
	// the runtime assigns it after a successful compile and before
	// calling ExecuteAppend.
	CompileAppend(ctx context.Context, req AppendRequest) CompileResult
	// ExecuteAppend performs the write. It assumes the request
	// has been Compiled and the ledger was AllNative; otherwise
	// the runtime will not call it.
	ExecuteAppend(ctx context.Context, req AppendRequest) (AppendResponse, error)
}
