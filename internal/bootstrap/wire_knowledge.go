package bootstrap

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/knowledge"

	otellog "go.opentelemetry.io/otel/log"
)

// wireKnowledge creates the FS-backed knowledge store, builds the initial
// index, and optionally starts the semantic processor and file watcher.
// The returned cleanup stops the watcher and semantic processor.
func wireKnowledge(ctx context.Context, ws workspace.Workspace, llmResolver llm.LLMResolver) (knowledge.Store, func(), error) {
	var cleanups []func()

	fsStore := knowledge.NewFSStore(ws)
	if err := fsStore.BuildIndex(ctx); err != nil {
		telemetry.Warn(ctx, "knowledge: initial index build failed", otellog.String("error", err.Error()))
	}

	semanticLLM, semErr := llmResolver.Resolve(ctx, "")
	knowledgeStore := knowledge.NewCachedStore(fsStore)

	var semProc *knowledge.SemanticProcessor
	if semErr == nil && semanticLLM != nil {
		semProc = knowledge.NewSemanticProcessor(semanticLLM, fsStore,
			knowledge.WithOnEvict(func(datasetID string) { knowledgeStore.EvictDataset(datasetID) }),
		)
		semProc.Start(ctx)
		fsStore.SetSemanticProcessor(semProc)
		cleanups = append(cleanups, func() { semProc.Stop() })
	} else {
		telemetry.Warn(ctx, "knowledge: SemanticProcessor disabled (no LLM configured), L0/L1 summaries will not be generated — knowledge base will use BM25 search only")
	}

	if kw, kwErr := fsStore.StartWatching(ctx); kwErr != nil {
		telemetry.Warn(ctx, "knowledge: file watcher failed to start", otellog.String("error", kwErr.Error()))
	} else if kw != nil {
		cleanups = append(cleanups, func() { kw.Stop() })
	}

	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	return knowledgeStore, cleanup, nil
}
