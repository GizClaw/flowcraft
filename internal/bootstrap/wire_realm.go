package bootstrap

import (
	"context"

	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/realm"
	"github.com/GizClaw/flowcraft/internal/sandbox"
	"github.com/GizClaw/flowcraft/sdk/graph/adapter"
	"github.com/GizClaw/flowcraft/sdk/graph/executor"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workflow"
	"github.com/GizClaw/flowcraft/sdk/workspace"

	otellog "go.opentelemetry.io/otel/log"
)

// wireRealm creates the memory subsystem, checkpoint store, graph executor,
// PlatformDeps, and SingleRealmProvider. It also registers memory tools.
// The returned cleanup closes the realm provider.
func wireRealm(
	ctx context.Context,
	store model.Store,
	cfg *config.Config,
	ws workspace.Workspace,
	factory *node.Factory,
	llmResolver llm.LLMResolver,
	toolReg *tool.Registry,
	sandboxCfg sandbox.ManagerConfig,
) (*realm.SingleRealmProvider, memory.LongTermStore, func(), error) {
	ltStore := memory.NewFileLongTermStore(ws, "", memory.WithMaxEntries(0))

	cpDir := config.CheckpointsDir()
	checkpointStore, err := executor.NewFileCheckpointStore(executor.FileCheckpointConfig{
		Dir:            cpDir,
		MaxCheckpoints: 3,
	})
	if err != nil {
		telemetry.Warn(ctx, "checkpoint store disabled", otellog.String("error", err.Error()))
		checkpointStore = nil
	}

	memStore := memory.NewFileStore(ws, "memory")
	summaryStore := memory.NewFileSummaryStore(ws, "memory")
	memoryFactory := func(ctx context.Context, mcfg model.MemoryConfig) (memory.Memory, error) {
		l, _ := llmResolver.Resolve(ctx, "")
		return memory.NewWithLLM(mcfg, memStore, l, ltStore,
			memory.WithWorkspace(ws),
			memory.WithPrefix("memory"),
		)
	}

	memory.RegisterTools(toolReg, memory.ToolDeps{
		SummaryStore: summaryStore,
		MessageStore: memStore,
		Workspace:    ws,
		Prefix:       "memory",
		Config:       memory.DefaultDAGConfig(),
	})

	memExtractor := memory.NewMemoryExtractor(llmResolver, ltStore, memory.LongTermConfig{Enabled: true}, memory.ExtractorConfig{})

	graphExecutor := executor.NewLocalExecutor()
	platformCfg := &realm.PlatformDeps{
		Store:           store,
		Factory:         factory,
		Executor:        graphExecutor,
		Workspace:       ws,
		MemoryFactory:   memoryFactory,
		Extractor:       memExtractor,
		SummaryStore:    summaryStore,
		CheckpointStore: checkpointStore,
		StrategyResolver: func(a *model.Agent) workflow.Strategy {
			if gd := a.StrategyDef.AsGraph(); gd != nil {
				return adapter.FromDefinition(gd)
			}
			return nil
		},
	}
	runtimeMgr := realm.NewSingleRealmProvider(store, platformCfg, sandboxCfg, sandboxCfg.IdleTimeout)

	return runtimeMgr, ltStore, runtimeMgr.Close, nil
}
