package config

import (
	"context"
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

var (
	ErrNotFound = errors.New("inference configuration not found")
	ErrConflict = errors.New("inference configuration revision conflict")
)

// AnyRevision requests an unconditional replacement. An empty expected
// revision creates only when no snapshot exists; any other value performs an
// optimistic compare-and-swap.
const AnyRevision = "*"

type Snapshot struct {
	Document Document
	Revision string
}

func (s Snapshot) Clone() Snapshot {
	s.Document = s.Document.Clone()
	return s
}

// Store persists complete configuration snapshots. Implementations must be
// safe for concurrent use. Save must implement the expected-revision contract
// documented by AnyRevision.
type Store interface {
	Load(context.Context) (Snapshot, error)
	Save(context.Context, string, Document) (Snapshot, error)
}

// ErrNotifyUnsupported marks stores that cannot push change notifications.
// Reloader.Watch silently falls back to interval polling for them.
var ErrNotifyUnsupported = errors.New(
	"store does not support change notification",
)

// Notifier is an optional Store capability: push-based change notification.
// Signals are advisory only — they may be missed (queue overflow, reconnect
// gaps) or spurious, and they never carry a revision. Correctness always
// comes from loading and comparing revisions; a signal is only a hint to
// reload now instead of waiting for the next poll. The returned channel is
// closed when the watch ends, and implementations must not block on send.
type Notifier interface {
	Notify(ctx context.Context) (<-chan struct{}, error)
}

func (b *Builder) LoadRuntime(
	ctx context.Context,
	store Store,
	options ...inference.RuntimeOption,
) (*inference.Runtime, Snapshot, error) {
	if isNilInterface(store) {
		return nil, Snapshot{}, fmt.Errorf(
			"inference configuration store is nil",
		)
	}
	snapshot, err := store.Load(ctx)
	if err != nil {
		return nil, Snapshot{}, err
	}
	runtime, err := b.NewRuntime(ctx, snapshot.Document, options...)
	if err != nil {
		return nil, Snapshot{}, err
	}
	return runtime, snapshot.Clone(), nil
}
