package dynamic

import (
	"context"

	sdktool "github.com/GizClaw/flowcraft/sdk/tool"
)

// WithCatalog attaches the session catalog to ctx so session-scoped
// tools (tool_search) can resolve it at execution time. The pattern
// rides the neutral sdk/tool context channel, so the inference node
// and the runtime session layer share one plumbing.
func WithCatalog(ctx context.Context, c *Catalog) context.Context {
	return sdktool.WithCatalogOnContext(ctx, c)
}

// FromContext returns the session catalog attached by WithCatalog.
func FromContext(ctx context.Context) (*Catalog, bool) {
	c, ok := sdktool.CatalogFromContext(ctx)
	if !ok {
		return nil, false
	}
	cat, ok := c.(*Catalog)
	return cat, ok
}
