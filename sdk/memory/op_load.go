package memory

import "context"

// LoadRequest reads records from a transcript. Cursor is the
// opaque continuation token returned by a previous LoadResponse;
// the kernel never parses it. Limit caps the result; Reverse
// flips the order.
//
// Limit is required for any non-toy deployment: a hook that
// omits Limit falls back to Spec.DefaultLoadLimit, then
// Spec.FallbackLoadLimit. A Load with no effective Limit is
// rejected at the Compile stage because unbounded transcript
// reads are too expensive to do per Run.
type LoadRequest struct {
	Scope          Scope
	ConversationID string
	// Cursor is the opaque continuation token. An empty Cursor
	// means "from the start (or end, when Reverse is true)".
	// Implementations define the cursor format; the kernel
	// treats it as an opaque string.
	Cursor string
	// Limit caps the number of returned records. 0 means
	// "no limit", which the runtime rejects in production
	// paths.
	Limit int
	// Reverse returns newest-first when true.
	Reverse bool
}

// LoadResponse is what Load returns. Records is in the order
// requested (oldest-first by default, newest-first when
// LoadRequest.Reverse is set). NextCursor is what callers pass
// to the next LoadRequest to continue; empty means "no more
// records".
type LoadResponse struct {
	Records    []Record
	NextCursor string
}

// LoadOp is the contract an implementation satisfies to handle the
// Load operation. As with all op interfaces, implementations do
// the read; the runtime wraps it with compile enforcement.
type LoadOp interface {
	CompileLoad(ctx context.Context, req LoadRequest) CompileResult
	ExecuteLoad(ctx context.Context, req LoadRequest) (LoadResponse, error)
}
