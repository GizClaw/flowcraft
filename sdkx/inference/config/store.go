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
