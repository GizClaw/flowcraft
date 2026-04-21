package bootstrap

import (
	"context"

	"github.com/GizClaw/flowcraft/internal/knowledgeproc"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/knowledge"

	otellog "go.opentelemetry.io/otel/log"
)

// wireKnowledge constructs the FS-backed knowledge store, builds the
// initial index, starts the file watcher, and stands up the
// platform-side context worker that owns LLM-driven L0/L1 generation.
//
// When llmResolver cannot produce an LLM the worker still runs in
// no-op mode: SubmitDocument immediately marks the document completed
// so REST consumers do not get stuck on "processing", preserving the
// historical BM25-only behaviour.
//
// The returned cleanup stops the watcher and the worker in
// reverse-init order.
func wireKnowledge(ctx context.Context, ws workspace.Workspace, llmResolver llm.LLMResolver, appStore model.Store) (knowledge.Store, *knowledgeproc.Worker, func(), error) {
	var cleanups []func()

	fsStore := knowledge.NewFSStore(ws)
	if err := fsStore.BuildIndex(ctx); err != nil {
		telemetry.Warn(ctx, "knowledge: initial index build failed", otellog.String("error", err.Error()))
	}

	cachedStore := knowledge.NewCachedStore(fsStore)

	var workerLLM llm.LLM
	if llmResolver != nil {
		l, err := llmResolver.Resolve(ctx, "")
		if err != nil {
			telemetry.Warn(ctx, "knowledge: LLM unavailable, context worker will run in no-op mode (BM25-only search)",
				otellog.String("error", err.Error()))
		} else {
			workerLLM = l
		}
	}

	worker := knowledgeproc.New(knowledgeproc.Deps{
		FSStore:     fsStore,
		CachedStore: cachedStore,
		AppStore:    appStore,
		LLM:         workerLLM,
	})
	worker.Start(ctx)
	cleanups = append(cleanups, worker.Stop)

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
	return cachedStore, worker, cleanup, nil
}
