package session

import (
	"context"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/tool"
)

// CatalogProvider constructs the per-session tool catalog. The provider
// runs once per Session (on its first Start) and its result is attached
// to every turn's execution context, where session-scoped tools
// (tool_search) and the inference node's all_tools mode pick it up. The
// Session borrows the catalog.
//
// The provider returns the session-scoped tool.Session (hosts typically
// use tool.Assembly.NewSession), so the dynamic injection view and the
// plain catalog share one contract.
type CatalogProvider interface {
	NewCatalog(ctx context.Context, instance *agent.Agent) (tool.Session, error)
}

// CatalogProviderFunc adapts a plain function to CatalogProvider.
type CatalogProviderFunc func(ctx context.Context, instance *agent.Agent) (tool.Session, error)

// NewCatalog implements CatalogProvider.
func (f CatalogProviderFunc) NewCatalog(ctx context.Context, instance *agent.Agent) (tool.Session, error) {
	return f(ctx, instance)
}
