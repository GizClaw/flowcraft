package memory

import (
	"context"
	"time"
)

// CompactRequest asks the runtime to roll a transcript window
// (everything older than OlderThan) into a summary, while keeping
// the most recent Keep messages verbatim. It is a lifecycle
// maintenance operation. ExecuteCompact performs the storage
// mutation; callers normally invoke it through the scheduler.
type CompactRequest struct {
	Scope     Scope
	OlderThan time.Time
	// Keep is the number of recent messages to preserve
	// verbatim. 0 means the runtime picks a default from the
	// lifecycle config.
	Keep int
}

// CompactResponse summarises the completed compaction. Compacted is
// the message count replaced with a summary; Bytes is the storage
// occupied by the result.
type CompactResponse struct {
	Compacted int
	Bytes     int64
}

// CompactOp is the mutating maintenance contract an implementation
// satisfies to compact storage.
type CompactOp interface {
	CompileCompact(ctx context.Context, req CompactRequest) CompileResult
	ExecuteCompact(ctx context.Context, req CompactRequest) (CompactResponse, error)
}
