package recall

import (
	"context"

	recallpipe "github.com/GizClaw/flowcraft/sdk/recall/pipeline"
)

// internalEntityLinkResolver bridges an [EntityStore] (Scope-keyed)
// to a [pipeline.EntityLinkResolver] (namespace-keyed). It is the
// glue that lets `sdk/retrieval/pipeline` consult an entity index
// living in `sdk/recall` WITHOUT creating an import cycle:
//
//	pipeline (cannot import recall) ── needs EntityLinkResolver
//	recall   (imports pipeline)     ── provides EntityLinkResolver
//	                                   adapter wrapping EntityStore.
//
// Lifecycle: [Memory.New] constructs one of these whenever
// [WithEntityStore] is enabled AND a non-nil [EntityStore] gets
// wired (i.e. the backing index satisfies retrieval.DocGetter).
// Callers do not see the type; the resolver flows through
// [pipeline.WithEntityLinkResolver] into [pipeline.LTM].
type internalEntityLinkResolver struct {
	store EntityStore
}

// newInternalEntityLinkResolver returns a resolver bound to `store`,
// or nil when `store` is nil so [pipeline.WithEntityLinkResolver]
// receives an explicit-nil interface (typed-nil interfaces would
// fail the `if resolver != nil` guard inside the stage).
func newInternalEntityLinkResolver(store EntityStore) recallpipe.EntityLinkResolver {
	if store == nil {
		return nil
	}
	return &internalEntityLinkResolver{store: store}
}

// ResolveLinks implements [pipeline.EntityLinkResolver]. It maps the
// pipeline's namespace-keyed surface back to a [Scope] via
// [ScopeFromNamespace] and delegates to the EntityStore.
//
// Namespace shape mismatches are non-fatal — a namespace that does
// not match the [NamespaceFor] grammar (e.g. legacy namespaces from
// before sdk/recall existed) returns an empty candidate set rather
// than an error, so the EntityLinkLookup stage simply produces no
// CandidateEntityIDs and the lane stays silent for that request.
func (r *internalEntityLinkResolver) ResolveLinks(
	ctx context.Context,
	namespace string,
	entities []string,
	perEntityCap int,
) ([]string, error) {
	scope, ok := ScopeFromNamespace(namespace)
	if !ok {
		return nil, nil
	}
	return r.store.Lookup(ctx, scope, entities, perEntityCap)
}
