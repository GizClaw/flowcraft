package recall

import (
	"context"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// entityStoreProjection wraps an [EntityStore] so the [Reconciler]
// can drive it as a generic [Projection].
//
// The adapter is intentionally thin: it expresses the contract that
// "the entity-link inverted index is a derived view of the entry
// namespace" in code rather than in a comment. Implementation notes:
//
//   - Project rebuilds the (entity → entry-id list) edge set from
//     entry.Entities for the supplied entries and pushes it via
//     [EntityStore.Link]. Link is idempotent on (entity, id), so
//     this can be replayed safely (the Reconciler does so every
//     tick).
//   - Forget loops [EntityStore.Forget] over the stale ids. Most
//     backends scan the entity namespace per call; for typical
//     LoCoMo-shaped scopes (tens of stale ids per reconcile after a
//     TTL sweep or rollback) the cost is negligible. Optimise via a
//     batch interface if production telemetry shows the loop is
//     hot.
//   - AllEntryIDs scans the entity namespace via the wrapped index
//     and unions every row's MetaEntityLinked list. The Reconciler
//     uses this to compute (retained - alive) and Forget the diff.
//     Bounded by total stored entity-rows under the scope.
type entityStoreProjection struct {
	store EntityStore
	idx   retrieval.Index // same backing index the EntityStore lives in; used by AllEntryIDs
}

// newEntityStoreProjection returns nil when store is nil or the
// backing index doesn't expose List — callers degrade gracefully
// to "feature disabled" (same posture as NewIndexEntityStore).
func newEntityStoreProjection(store EntityStore, idx retrieval.Index) *entityStoreProjection {
	if store == nil || idx == nil {
		return nil
	}
	return &entityStoreProjection{store: store, idx: idx}
}

// Name implements Projection.
func (p *entityStoreProjection) Name() string { return "entity_store" }

// Project implements Projection.
//
// Builds the edge set from entries[*].Entities — the same field
// upsertFacts' eager linkEntities path consumes — and replays it
// into the EntityStore. Idempotent because EntityStore.Link dedups
// (entity, id) pairs and FIFO-evicts only when the row exceeds the
// configured cap. Empty entry slices and entries without entities
// are no-ops.
func (p *entityStoreProjection) Project(ctx context.Context, scope Scope, entries []Entry) error {
	if p == nil || p.store == nil || len(entries) == 0 {
		return nil
	}
	edges := make(map[string][]string, len(entries))
	for _, e := range entries {
		for _, ent := range e.Entities {
			ent = strings.TrimSpace(ent)
			if ent == "" {
				continue
			}
			edges[ent] = append(edges[ent], e.ID)
		}
	}
	if len(edges) == 0 {
		return nil
	}
	return p.store.Link(ctx, scope, edges)
}

// Forget implements Projection. EntityStore.Forget is per-id, so
// we loop. The first error short-circuits — partial Forgets leave
// the projection consistent enough (other ids may remain stale
// until the next reconcile) but the Reconciler logs the failure so
// operators can investigate.
func (p *entityStoreProjection) Forget(ctx context.Context, scope Scope, entryIDs []string) error {
	if p == nil || p.store == nil || len(entryIDs) == 0 {
		return nil
	}
	for _, id := range entryIDs {
		if id == "" {
			continue
		}
		if err := p.store.Forget(ctx, scope, id); err != nil {
			return err
		}
	}
	return nil
}

// AllEntryIDs implements ProjectionInspector. Walks every row under
// the entity namespace and unions the MetaEntityLinked lists.
// Result order is unspecified (deduped by caller). Returns nil for
// scopes the EntityStore has never seen.
func (p *entityStoreProjection) AllEntryIDs(ctx context.Context, scope Scope) ([]string, error) {
	if p == nil || p.idx == nil {
		return nil, nil
	}
	ns := EntityNamespaceFor(scope)
	seen := make(map[string]struct{})
	var page string
	for {
		resp, err := p.idx.List(ctx, ns, retrieval.ListRequest{
			PageSize:  500,
			PageToken: page,
		})
		if err != nil || resp == nil {
			return nil, err
		}
		for _, d := range resp.Items {
			for _, id := range entityLinkedSlice(d) {
				if id != "" {
					seen[id] = struct{}{}
				}
			}
		}
		if resp.NextPageToken == "" {
			break
		}
		page = resp.NextPageToken
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// Compile-time interface checks.
var (
	_ Projection          = (*entityStoreProjection)(nil)
	_ ProjectionInspector = (*entityStoreProjection)(nil)
)
