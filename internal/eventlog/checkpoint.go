package eventlog

import (
	"context"
)

// CheckpointStore manages projector read cursors.
type CheckpointStore interface {
	// Get returns the checkpoint seq for a projector. Returns (0, nil) if none.
	Get(ctx context.Context, projectorName string) (int64, error)
	// List returns all projector checkpoints.
	List(ctx context.Context) (map[string]int64, error)
	// Min returns the minimum checkpoint across all projectors.
	// Returns (0, false) if no projectors have checkpoints.
	Min(ctx context.Context) (int64, bool, error)
}
