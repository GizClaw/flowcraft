package memory

import (
	"context"
	"time"
)

// ArchiveRequest asks the runtime to move a compacted transcript
// window (everything older than OlderThan) to a cold destination.
// ExecuteArchive performs the move; callers normally invoke it
// through the maintenance scheduler.
//
// Destination is opaque: a path, an S3 URI, or any locator the
// implementation can write to. The kernel never inspects it.
type ArchiveRequest struct {
	Scope       Scope
	OlderThan   time.Time
	Destination string
}

// ArchiveResponse summarises the completed move. Archived is the
// message count shipped; Bytes is the storage written.
type ArchiveResponse struct {
	Archived int
	Bytes    int64
}

// ArchiveOp is the mutating maintenance contract an implementation
// satisfies to move records to cold storage.
type ArchiveOp interface {
	CompileArchive(ctx context.Context, req ArchiveRequest) CompileResult
	ExecuteArchive(ctx context.Context, req ArchiveRequest) (ArchiveResponse, error)
}
