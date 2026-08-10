package tool

import "context"

type catalogContextKey struct{}

// WithCatalogOnContext attaches a session-scoped Catalog to ctx. It is
// the neutral plumbing between a session owner (sdkx/runtime/session),
// model-facing consumers (the inference node), and session-scoped
// tools (tool_search). The attached catalog is a view, not an owner:
// whoever creates it is responsible for closing it.
func WithCatalogOnContext(ctx context.Context, c Catalog) context.Context {
	return context.WithValue(ctx, catalogContextKey{}, c)
}

// CatalogFromContext returns the session catalog attached by
// WithCatalogOnContext. The bool is false when no catalog is attached
// (or when a typed nil was stored — treat that as absent).
func CatalogFromContext(ctx context.Context) (Catalog, bool) {
	c, ok := ctx.Value(catalogContextKey{}).(Catalog)
	if !ok {
		return nil, false
	}
	if c == nil {
		return nil, false
	}
	return c, true
}
