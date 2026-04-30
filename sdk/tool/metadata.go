package tool

// ToolMeta carries optional, sandbox-relevant metadata about a Tool.
//
// All fields are advisory; a zero ToolMeta means "no claims, treat
// conservatively" (no rate limit, assume the tool may mutate state so
// retries are unsafe).
//
// The shape is intentionally minimal — only fields a pod-side sandbox
// can act on today are present:
//
//   - RateLimit drives request-per-second throttling middleware.
//   - MutatesState gates whether retry-on-failure logic may safely
//     re-invoke the tool with the same arguments.
//
// Network / filesystem / cost claims were deliberately deferred:
// in-process pods cannot enforce process-boundary isolation
// (see docs/sdk-pod-runtime-gaps.md §3.5), and $-denominated cost
// caps presuppose the LLM pricing catalog that is also deferred.
// Add fields here when (and only when) a concrete sandbox component
// is ready to consume them.
type ToolMeta struct {
	// RateLimit is the maximum number of executions per second this
	// tool can sustain. Zero means "no claim" (no rate limit applied).
	// A negative value is treated as zero.
	RateLimit float64

	// MutatesState declares that this tool has side effects beyond
	// returning a result (writes, posts, sends mail, ...). Conservative
	// callers (retry middleware, "redo last call" prompts) should
	// refuse to re-invoke a MutatesState tool with the same input
	// without explicit user confirmation.
	//
	// Zero value (false) is the conservative default in the *opposite*
	// direction: callers that don't know better should assume the tool
	// MAY mutate state. Tools that are provably side-effect free should
	// declare MutatesState=false explicitly via Metadata().
	MutatesState bool
}

// ToolMetadata is an optional interface a Tool may implement to
// declare sandbox-relevant metadata. Tools that do not implement
// this interface are treated as if they returned a zero ToolMeta
// (no rate limit, side-effects unknown).
type ToolMetadata interface {
	Metadata() ToolMeta
}

// MetadataOf returns the ToolMeta declared by t, or a zero ToolMeta
// if t does not implement ToolMetadata. Safe to call on any Tool,
// including nil-interface values (returns zero ToolMeta).
func MetadataOf(t Tool) ToolMeta {
	if t == nil {
		return ToolMeta{}
	}
	if m, ok := t.(ToolMetadata); ok {
		return m.Metadata()
	}
	return ToolMeta{}
}
