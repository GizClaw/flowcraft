package session

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

// CatalogProvider constructs the per-session tool catalog. The provider
// runs once per Session (on its first Start) and its result is attached
// to every turn's execution context, where session-scoped tools
// (tool_search) and the inference node's all_tools mode pick it up. The
// Session owns the catalog: it is closed exactly once when the Session
// closes, via the optional io.Closer contract.
//
// The provider is deliberately generic — it returns tool.Catalog, not a
// dynamic catalog — so hosts can supply a dynamic injection view, a
// filtered view, or nothing at all without the runtime knowing which.
type CatalogProvider interface {
	NewCatalog(ctx context.Context, instance *deploy.Instance) (tool.Catalog, error)
}

// CatalogProviderFunc adapts a plain function to CatalogProvider.
type CatalogProviderFunc func(ctx context.Context, instance *deploy.Instance) (tool.Catalog, error)

// NewCatalog implements CatalogProvider.
func (f CatalogProviderFunc) NewCatalog(ctx context.Context, instance *deploy.Instance) (tool.Catalog, error) {
	return f(ctx, instance)
}
