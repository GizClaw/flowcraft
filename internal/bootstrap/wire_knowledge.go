package bootstrap

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/knowledge"

	otellog "go.opentelemetry.io/otel/log"
)

// wireKnowledge creates the FS-backed knowledge store, builds the initial
// index, and starts the file watcher. The returned cleanup stops the
// watcher.
//
// After the sdkx Scheme E refactor, layered context (L0/L1) generation is no
// longer driven by the SDK. A platform-side worker (introduced in a follow-up
// commit) will own LLM orchestration and persistence of l0_abstract,
// l1_overview and processing_status back into the app store.
func wireKnowledge(ctx context.Context, ws workspace.Workspace) (knowledge.Store, func(), error) {
	var cleanups []func()

	fsStore := knowledge.NewFSStore(ws)
	if err := fsStore.BuildIndex(ctx); err != nil {
		telemetry.Warn(ctx, "knowledge: initial index build failed", otellog.String("error", err.Error()))
	}

	knowledgeStore := knowledge.NewCachedStore(fsStore)

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
