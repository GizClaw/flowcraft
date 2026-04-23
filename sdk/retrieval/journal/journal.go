package journal

import (
	"context"
	"iter"
	"time"
)

// Journal records index mutations for audit and replay.
type Journal interface {
	Record(ctx context.Context, ev Event) error
	History(ctx context.Context, namespace, docID string) ([]Event, error)
	Replay(ctx context.Context, namespace string, sinceSeq uint64) iter.Seq2[Event, error]
	Compact(ctx context.Context, before time.Time) error
	Close() error
}
