package runtime

import (
	"context"

	"github.com/GizClaw/flowcraft/core/tool/engine"
)

// NewRunContext returns ctx carrying a fresh catalog view for one run.
//
// The view remembers what the model discovered and how many rounds have passed,
// so it belongs to one run: create it when the run starts and drop it when the
// run ends. It must not live on a Runtime, because two conversations would then
// share one discovery pool.
func (r *Runtime) NewRunContext(ctx context.Context) context.Context {
	return engine.WithCatalogView(ctx, r.NewCatalogView())
}

// WithCatalogViewCtx attaches an existing view. A caller that keeps a view per
// conversation rather than per run reuses it through this.
func WithCatalogViewCtx(ctx context.Context, view engine.CatalogView) context.Context {
	return engine.WithCatalogView(ctx, view)
}

// CatalogViewFrom returns the view attached to ctx, if any.
func CatalogViewFrom(ctx context.Context) (engine.CatalogView, bool) {
	return engine.CatalogViewFrom(ctx)
}
