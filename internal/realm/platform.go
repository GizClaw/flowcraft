package realm

import (
	"context"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/graph/adapter"
	"github.com/GizClaw/flowcraft/sdk/graph/executor"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/memory"
	sdkmodel "github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workflow"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// MemoryFactory creates a Memory instance from an agent's memory config.
type MemoryFactory func(ctx context.Context, cfg model.MemoryConfig) (memory.Memory, error)

// StrategyResolver resolves a workflow.Strategy for the given agent.
type StrategyResolver func(agent *model.Agent) workflow.Strategy

// PlatformDeps holds all platform-specific dependencies needed by a Realm.
// It is a pure dependency container with no execution logic.
type PlatformDeps struct {
	Store            model.Store
	Factory          *node.Factory
	Executor         executor.Executor
	Workspace        workspace.Workspace
	MemoryFactory    MemoryFactory
	Extractor        *memory.MemoryExtractor
	SummaryStore     memory.SummaryStore
	CheckpointStore  executor.CheckpointStore
	StrategyResolver StrategyResolver

	// RunOverride, when set, replaces the entire runtime.Run path (used in tests).
	RunOverride func(ctx context.Context, agent *model.Agent, req *workflow.Request) (*workflow.Result, error)
}

// BuildRuntime constructs a workflow.Runtime from platform deps.
func (d *PlatformDeps) BuildRuntime() workflow.Runtime {
	deps := workflow.NewDependencies()
	if d.Factory != nil {
		workflow.SetDep(deps, adapter.DepNodeFactory, d.Factory)
	}
	if d.Executor != nil {
		workflow.SetDep(deps, adapter.DepExecutor, d.Executor)
	}

	var opts []workflow.RuntimeOption
	opts = append(opts, workflow.WithDependencies(deps))

	if d.MemoryFactory != nil {
		opts = append(opts, workflow.WithMemoryFactory(bridgeMemoryFactory(d.MemoryFactory)))
	}

	return workflow.NewRuntime(opts...)
}

// ---------- memory bridge ----------

// bridgeMemoryFactory adapts the platform MemoryFactory to SDK's workflow.MemoryFactory.
func bridgeMemoryFactory(factory MemoryFactory) workflow.MemoryFactory {
	return func(ctx context.Context, agent workflow.Agent) (workflow.Memory, error) {
		a, ok := agent.(*model.Agent)
		if !ok {
			return nil, nil
		}
		mem, err := factory(ctx, a.Config.Memory)
		if err != nil {
			return nil, err
		}
		if mem == nil {
			return nil, nil
		}
		if aware, ok := mem.(*memory.MemoryAwareMemory); ok {
			aware.SetRuntimeID(model.RuntimeIDFrom(ctx))
			sc := memoryScope(ctx, &workflow.Request{
				RuntimeID: model.RuntimeIDFrom(ctx),
				ContextID: memory.ConversationIDFrom(ctx),
			})
			aware.SetScope(&sc)
		}
		return &memoryBridge{inner: mem}, nil
	}
}

type memoryBridge struct {
	inner memory.Memory
}

func (b *memoryBridge) Session(_ context.Context, contextID string) (workflow.MemorySession, error) {
	return &memorySessionBridge{inner: b.inner, contextID: contextID}, nil
}

type memorySessionBridge struct {
	inner     memory.Memory
	contextID string
}

func (s *memorySessionBridge) Load(ctx context.Context) ([]sdkmodel.Message, error) {
	return s.inner.Load(ctx, s.contextID)
}

func (s *memorySessionBridge) Vars(context.Context) (map[string]any, error) { return nil, nil }

func (s *memorySessionBridge) Save(ctx context.Context, msgs []sdkmodel.Message) error {
	return s.inner.Save(ctx, s.contextID, msgs)
}

func (s *memorySessionBridge) Close(context.Context, error) error { return nil }

// ---------- helpers ----------

func memoryScope(ctx context.Context, req *workflow.Request) memory.MemoryScope {
	uid := model.RuntimeIDFrom(ctx)
	if uid == "" {
		uid = req.RuntimeID
	}
	sid := memory.ConversationIDFrom(ctx)
	if sid == "" {
		sid = req.ContextID
	}
	return memory.MemoryScope{
		UserID:    uid,
		SessionID: sid,
	}
}
