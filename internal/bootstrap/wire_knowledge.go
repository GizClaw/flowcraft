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
// When llmResolver cannot produce an LLM the worker is not
// constructed and the returned worker pointer is nil. Callers must
// guard the worker with KnowledgeWorker == nil; the API surface
// rejects context-generating writes (AddDocument, ReprocessDocument)
// in that mode rather than silently degrading.
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
			telemetry.Warn(ctx, "knowledge: LLM unavailable, context worker disabled (BM25-only search; AddDocument/Reprocess will return 503)",
				otellog.String("error", err.Error()))
		} else {
			workerLLM = l
		}
	}

	var worker *knowledgeproc.Worker
	if workerLLM != nil {
		w, err := knowledgeproc.New(knowledgeproc.Deps{
			FSStore:     fsStore,
			CachedStore: cachedStore,
			AppStore:    appStore,
			LLM:         workerLLM,
		})
		if err != nil {
			return nil, nil, nil, err
		}
		w.Start(ctx)
		cleanups = append(cleanups, w.Stop)
		worker = w
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
	return cachedStore, worker, cleanup, nil
}
