package workspace

import (
	"context"
	"errors"
	"sync"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

const (
	stateFile = "state.json"
	stateTmp  = "state.json.tmp"
)

// Store is the durable canonical ledger plus its optional scope enumerator.
type Store interface {
	recall.TemporalStore
	recall.ScopeEnumerator
}

// Backend owns the workspace subtree shared by recall's durable adapters.
type Backend struct {
	mu sync.Mutex
	ws sdkworkspace.Workspace
}

// Option configures a workspace backend.
type Option func(*Backend)

// Open creates a LocalWorkspace-backed recall durable backend at dir.
func Open(dir string, opts ...Option) (*Backend, error) {
	ws, err := sdkworkspace.NewLocalWorkspace(dir)
	if err != nil {
		return nil, err
	}
	return New(ws, opts...)
}

// New creates a recall durable backend on an existing Workspace.
func New(ws sdkworkspace.Workspace, opts ...Option) (*Backend, error) {
	if ws == nil {
		return nil, errdefs.Validationf("recall workspace: nil workspace")
	}
	b := &Backend{ws: ws}
	for _, opt := range opts {
		opt(b)
	}
	return b, nil
}

// Close implements the same ownership shape as other recall store backends.
func (b *Backend) Close() error { return nil }

// TemporalStore returns the canonical fact ledger adapter.
func (b *Backend) TemporalStore() Store { return &temporalStore{b: b} }

// SideEffectOutbox returns the commit-after side-effect outbox adapter.
func (b *Backend) SideEffectOutbox() recall.SideEffectOutbox { return &sideEffectOutbox{b: b} }

// AsyncSemanticQueue returns the async semantic durable queue adapter.
func (b *Backend) AsyncSemanticQueue() recall.AsyncSemanticQueue {
	return &asyncSemanticQueue{b: b}
}

// EvidenceStore returns the secondary evidence lookup adapter.
func (b *Backend) EvidenceStore() recall.EvidenceStore { return &evidenceStore{b: b} }

func (b *Backend) load(ctx context.Context) (state, error) {
	raw, err := b.ws.Read(ctx, stateFile)
	if err != nil {
		if errors.Is(err, sdkworkspace.ErrNotFound) {
			return newState(), nil
		}
		return state{}, err
	}
	return decodeState(raw)
}

func (b *Backend) save(ctx context.Context, st state) error {
	raw, err := encodeState(st)
	if err != nil {
		return err
	}
	if err := b.ws.Write(ctx, stateTmp, raw); err != nil {
		return err
	}
	return b.ws.Rename(ctx, stateTmp, stateFile)
}

func samePartition(a, b domain.Scope) bool {
	return a.RuntimeID == b.RuntimeID && a.UserID == b.UserID
}

func factKey(scope domain.Scope, id string) string {
	return scope.PartitionKey() + "/" + id
}

func scopeFromPartition(scope domain.Scope) domain.Scope {
	return domain.Scope{RuntimeID: scope.RuntimeID, UserID: scope.UserID}
}

func ensurePartition(scope domain.Scope, op string) error {
	if scope.PartitionKey() == "" {
		return errdefs.Validationf("%s: scope partition is required (RuntimeID and UserID)", op)
	}
	return nil
}

var _ port.TemporalStore = (*temporalStore)(nil)
var _ port.ScopeEnumerator = (*temporalStore)(nil)
var _ port.EvidenceStore = (*evidenceStore)(nil)
var _ port.SideEffectOutbox = (*sideEffectOutbox)(nil)
var _ port.AsyncSemanticQueue = (*asyncSemanticQueue)(nil)
