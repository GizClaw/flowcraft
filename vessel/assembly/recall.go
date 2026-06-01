package assembly

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/ops"
	recallworkspace "github.com/GizClaw/flowcraft/memory/recall/store/workspace"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

type recallBundle struct {
	memory     recall.Memory
	enumerator recall.ScopeEnumerator
	closers    []func(context.Context) error
}

func buildRecall(ctx context.Context, spec *RecallSpec, ws WorkspaceHandle, defaults Defaults, catalog *Catalog, manifest Manifest) (recallBundle, error) {
	if spec == nil {
		return recallBundle{}, nil
	}
	backend, err := resolveRecallBackend(spec.Backend, defaults, catalog)
	if err != nil {
		return recallBundle{}, err
	}
	res, err := backend.BuildRecall(ctx, *spec, BackendDeps{Manifest: manifest, Workspace: ws})
	if err != nil {
		return recallBundle{}, err
	}
	return recallBundle{memory: res.Memory, enumerator: res.ScopeEnumerator, closers: res.Closers}, nil
}

type memoryRecallBackend struct{}

func MemoryRecallBackend() RecallBackend { return memoryRecallBackend{} }

func (memoryRecallBackend) ValidateRecall(RecallSpec) error { return nil }

func (memoryRecallBackend) BuildRecall(_ context.Context, spec RecallSpec, _ BackendDeps) (RecallResource, error) {
	opts := []recall.Option{recall.WithEvidenceStore(recall.NewMemoryEvidenceStore())}
	if spec.AsyncSemantic {
		opts = append(opts, recall.WithAsyncSemanticQueue(recall.NewInMemoryAsyncSemanticQueue()))
	}
	mem, err := recall.New(opts...)
	if err != nil {
		return RecallResource{}, err
	}
	return RecallResource{
		Memory:  mem,
		Closers: []func(context.Context) error{func(context.Context) error { return mem.Close() }},
	}, nil
}

type workspaceRecallBackend struct{}

func WorkspaceRecallBackend() RecallBackend { return workspaceRecallBackend{} }

func (workspaceRecallBackend) ValidateRecall(RecallSpec) error { return nil }

func (workspaceRecallBackend) BuildRecall(_ context.Context, spec RecallSpec, deps BackendDeps) (RecallResource, error) {
	if deps.Workspace == nil {
		return RecallResource{}, fmt.Errorf("vessel assembly: recall workspace backend requires workspace")
	}
	b, err := recallworkspace.New(sdkworkspace.Sub(deps.Workspace, defaultString(spec.Prefix, "recall")))
	if err != nil {
		return RecallResource{}, err
	}
	temporal := b.TemporalStore()
	enumerator, _ := temporal.(recall.ScopeEnumerator)
	opts := []recall.Option{
		recall.WithTemporalStore(temporal),
		recall.WithEvidenceStore(b.EvidenceStore()),
		recall.WithSideEffectOutbox(b.SideEffectOutbox()),
	}
	if spec.AsyncSemantic || spec.Ops.Enabled {
		opts = append(opts, recall.WithAsyncSemanticQueue(b.AsyncSemanticQueue()))
	}
	mem, err := recall.New(opts...)
	if err != nil {
		_ = b.Close()
		return RecallResource{}, err
	}
	return RecallResource{
		Memory:          mem,
		ScopeEnumerator: enumerator,
		Closers: []func(context.Context) error{
			func(context.Context) error { return mem.Close() },
			func(context.Context) error { return b.Close() },
		},
	}, nil
}

func buildRecallOps(m Manifest, spec *RecallSpec, bundle recallBundle) (*ops.Runner, ops.Target, error) {
	if spec == nil || !spec.Ops.Enabled || bundle.memory == nil {
		return nil, ops.Target{}, nil
	}
	options := []ops.Option{}
	if spec.Ops.WorkerID != "" {
		options = append(options, ops.WithWorkerID(spec.Ops.WorkerID))
	}
	if spec.Ops.BatchSize > 0 {
		options = append(options, ops.WithBatchSize(spec.Ops.BatchSize))
	}
	if spec.Ops.IdleInterval > 0 || spec.Ops.ErrorBackoff > 0 {
		options = append(options, ops.WithIntervals(spec.Ops.IdleInterval, spec.Ops.ErrorBackoff))
	}
	if spec.Ops.MaxConcurrentScopes > 0 {
		options = append(options, ops.WithMaxConcurrentScopes(spec.Ops.MaxConcurrentScopes))
	}
	if bundle.enumerator != nil {
		options = append(options, ops.WithScopeEnumerator(bundle.enumerator))
	}
	runner, err := ops.NewRunner(bundle.memory, options...)
	if err != nil {
		return nil, ops.Target{}, err
	}
	target := ops.Target{RuntimeID: m.ID}
	if len(spec.Ops.Scopes) > 0 {
		target.RuntimeID = ""
		target.Scopes = make([]recall.Scope, 0, len(spec.Ops.Scopes))
		for _, s := range spec.Ops.Scopes {
			runtimeID := s.RuntimeID
			if runtimeID == "" {
				runtimeID = m.ID
			}
			target.Scopes = append(target.Scopes, recall.Scope{
				RuntimeID: runtimeID,
				AgentID:   s.AgentID,
				UserID:    s.UserID,
			})
		}
	}
	return runner, target, nil
}
