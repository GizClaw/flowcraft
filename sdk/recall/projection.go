package recall

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/sdk/internal/background"
)

// Projection is a derived view of the primary recall index.
//
// Side stores (EntityStore today; GraphStore / SearchAnalytics
// tomorrow) implement Projection so the [Reconciler] can keep them
// eventually consistent with the primary index. The architectural
// contract is:
//
//   - The primary recall index is the SOURCE OF TRUTH for entries.
//     A fact exists iff its retrieval.Doc is alive (not tombstoned,
//     not expired) under the entry namespace.
//   - A Projection is a DERIVED VIEW. It accelerates one query
//     pattern (the entity-link inverted index is the canonical
//     example). It MAY lag, it MAY be missing entries, it MAY
//     retain stale entries — but reads must validate against the
//     primary and the [Reconciler] eventually converges the view.
//   - Eager write paths ([Memory.Save] via upsertFacts and
//     [Memory.Add]) call the replacement-capable projection path
//     inline AFTER the durable Upsert so the caller observes
//     0-lag recall against their own writes. If a projection
//     implements [ProjectionReplacer], the entry's derived state
//     is replaced with exactly what Entry currently says; otherwise
//     the legacy additive [Projection.Project] contract is used.
//   - [Memory.Forget] fans out through every registered Projection's
//     Forget method after the primary delete succeeds. Other
//     non-eager mutation paths (Rollback, TTL sweeper, resolver
//     OpDelete, …) rely on the [Reconciler]'s tick. A reconciler
//     pass is a full replay over alive primary entries, using
//     [ProjectionReplacer] when available and [Projection.Forget]
//     for IDs absent from primary.
//
// Implementations MUST be idempotent: Project called twice with the
// same scope + entries produces the same view state as once. Forget
// called for an ID that the view never had is a no-op.
//
// Concurrency: Projection methods must be safe for concurrent use
// by the Reconciler loop and by ad-hoc [Memory.SyncSideStores]
// callers.
type Projection interface {
	// Name is used for logs / metrics and to distinguish multiple
	// projections registered against the same Memory.
	Name() string

	// Project conveys "these entries currently exist (alive, not
	// tombstoned/expired) in the primary index, under scope".
	//
	// Semantics are ADDITIVE: the projection MUST incorporate
	// every edge implied by entries[*] (upsert new edges, refresh
	// existing ones) but MUST NOT drop edges that are absent from
	// the supplied slice — the caller may be the eager write path
	// passing only one batch, not a full snapshot. Stale-edge
	// cleanup is the [Reconciler]'s responsibility, driven by
	// [Projection.Forget] in a separate phase computed against
	// [ProjectionInspector.AllEntryIDs].
	//
	// Called from two paths sharing this exact contract: eager
	// write paths (Save's upsertFacts, Add) AFTER the durable
	// Upsert, and the [Reconciler] tick. Idempotent.
	Project(ctx context.Context, scope Scope, entries []Entry) error

	// Forget conveys "these entry IDs are no longer alive in
	// primary (deleted, expired, or rolled back to before they
	// existed)". The projection MUST drop any references to these
	// IDs. Idempotent on (scope, id) — calling Forget for an ID
	// the projection never knew about is a no-op.
	Forget(ctx context.Context, scope Scope, entryIDs []string) error
}

// ProjectionReplacer is an optional extension to [Projection] for
// per-entry replacement.
//
// Replace conveys "for each supplied entry, the projection state owned
// by that entry ID must now equal the state implied by Entry". For the
// entity-link projection, that means the entry ID appears under exactly
// Entry.Entities: old entity rows that still referenced the alive entry
// are pruned, and current rows are linked. This is stronger than
// additive [Projection.Project] and is what lets eager writes and
// Reconciler repair stale edges when an existing entry's Entities change.
//
// Idempotent on (scope, entry.ID). Empty entries and entries with no
// projected fields remove that entry's old projection edges.
type ProjectionReplacer interface {
	Replace(ctx context.Context, scope Scope, entries []Entry) error
}

// ProjectionInspector is an optional extension to [Projection]. The
// Reconciler calls AllEntryIDs to learn which entry IDs the view
// currently retains, so it can compute the set difference against
// the primary's alive set and call [Projection.Forget] on the
// stale IDs.
//
// Projections that do not implement this interface only get
// additive sync from the Reconciler (Project on alive entries);
// stale references in the view will not be cleaned up
// automatically — those projections must invoke Forget themselves
// from the write paths they care about.
type ProjectionInspector interface {
	// AllEntryIDs returns the union of entry IDs the projection
	// currently references under scope. Ordering is unspecified.
	// Duplicates are tolerated (the Reconciler dedupes).
	AllEntryIDs(ctx context.Context, scope Scope) ([]string, error)
}

// Reconciler is the background loop that brings each registered
// [Projection] into eventual consistency with the primary recall
// index. It owns the contract spelled out in the Projection godoc:
// projections lag, but the lag is bounded by the reconcile
// interval, and reads remain correct because the read path filters
// against the primary independently.
//
// Reconcile is per-namespace: the loop walks every namespace known
// to the configured [NamespaceRegistry], collects the alive
// entries under each, and asks every projection to (1) accept the
// alive set via Project and (2) drop any retained IDs that are not
// in the alive set via Forget.
//
// The Reconciler is constructed and owned by [Memory]; user code
// interacts with it only through [Memory.SyncSideStores], which
// runs one synchronous tick (used by tests and by callers who need
// 0-lag after a known write).
type Reconciler struct {
	primary     retrieval.Index
	projections []Projection
	registry    NamespaceRegistry
	interval    time.Duration
	batchSize   int
	now         func() time.Time
	log         func(format string, args ...any)

	group *background.Group
}

// newReconciler is the package-internal constructor. Callers go
// through [Memory] (recall.New); the Reconciler is wired into the
// Memory worker pool and shares its lifecycle.
func newReconciler(
	primary retrieval.Index,
	projections []Projection,
	registry NamespaceRegistry,
	interval time.Duration,
	now func() time.Time,
	log func(format string, args ...any),
) *Reconciler {
	if log == nil {
		log = func(string, ...any) {}
	}
	if now == nil {
		now = time.Now
	}
	if interval <= 0 {
		interval = defaultReconcileInterval
	}
	return &Reconciler{
		primary:     primary,
		projections: projections,
		registry:    registry,
		interval:    interval,
		batchSize:   defaultReconcileBatch,
		now:         now,
		log:         log,
		group:       background.NewGroup(context.Background()),
	}
}

// defaultReconcileInterval is the gap between background ticks
// when WithReconcileInterval is not set. 5 minutes is a compromise
// between freshness (#171 Add-then-Recall sees the new entry
// within 5 min) and cost (one full namespace scan per tick).
// Operators with large active scope counts should raise this.
const defaultReconcileInterval = 5 * time.Minute

// defaultReconcileBatch caps how many entries the Reconciler holds
// in memory per scope while computing the diff. Set high enough
// that medium scopes (hundreds of entries/scope) fit in one pass
// while still bounding RSS for production-scale namespaces.
const defaultReconcileBatch = 2000

// start launches the background tick goroutine. Memory.Close stops
// the loop via the shared stop channel.
func (r *Reconciler) start() {
	if r == nil || len(r.projections) == 0 {
		return
	}
	r.group.Start(r.loop)
}

// stop cancels the tick goroutine and waits for it to drain.
func (r *Reconciler) stop() {
	if r == nil {
		return
	}
	r.group.Close()
}

func (r *Reconciler) loop(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

// tick runs one full reconcile pass across every registered
// namespace. Errors per-scope are logged but do not abort the
// pass — a single misbehaving scope must not stall the others.
func (r *Reconciler) tick(ctx context.Context) {
	if r == nil || r.registry == nil || len(r.projections) == 0 {
		return
	}
	nss, err := r.registry.List(ctx)
	if err != nil {
		r.log("recall: reconcile namespaces list: %v", err)
		return
	}
	for _, ns := range nss {
		if err := ctx.Err(); err != nil {
			return
		}
		scope, ok := ScopeFromNamespace(ns)
		if !ok {
			// Sibling / unknown namespace (e.g. "..__entities");
			// skip — the entry namespace's reconcile will cover it.
			continue
		}
		scopeCtx, cancel := context.WithTimeout(ctx, r.interval)
		err := r.reconcileScope(scopeCtx, scope)
		cancel()
		if err != nil {
			r.log("recall: reconcile scope %q: %v", ns, err)
		}
	}
}

// SyncScope runs one synchronous reconcile pass for a single
// scope. Exposed via [Memory.SyncSideStores] so callers that just
// performed a write outside the eager hot path (Add, Rollback,
// Forget, TTL sweep, resolver OpDelete) can flush side stores to
// 0-lag instead of waiting for the next background tick.
func (r *Reconciler) SyncScope(ctx context.Context, scope Scope) error {
	if r == nil || len(r.projections) == 0 {
		return nil
	}
	return r.reconcileScope(ctx, scope)
}

func (r *Reconciler) reconcileScope(ctx context.Context, scope Scope) error {
	ns := NamespaceFor(scope)
	// 1. Pull alive entries from primary. "Alive" = not tombstoned
	// AND not expired-as-of-now. Reconciler MUST use the same
	// filter Recall uses (TombstoneFilter + ExpireFilter); a
	// projection that thinks an expired doc is still alive would
	// be more wrong than a projection that thinks an alive doc is
	// stale.
	now := r.now()
	filter := MergeFilters(TombstoneFilter(), ExpireFilter(now))
	aliveDocs, err := listAll(ctx, r.primary, ns, filter, r.batchSize)
	if err != nil {
		return err
	}
	aliveEntries := make([]Entry, 0, len(aliveDocs))
	aliveIDs := make(map[string]struct{}, len(aliveDocs))
	for _, d := range aliveDocs {
		e := DocToEntry(d)
		aliveEntries = append(aliveEntries, e)
		aliveIDs[e.ID] = struct{}{}
	}
	// 2. Fan out to projections.
	for _, p := range r.projections {
		if err := projectReplaceOrAdd(ctx, p, scope, aliveEntries); err != nil {
			r.log("recall: projection %s sync: %v", p.Name(), err)
		}
		// 3. Diff against the projection's retained IDs and Forget
		// anything the projection has that primary does not.
		insp, ok := p.(ProjectionInspector)
		if !ok {
			continue
		}
		retained, err := insp.AllEntryIDs(ctx, scope)
		if err != nil {
			r.log("recall: projection %s AllEntryIDs: %v", p.Name(), err)
			continue
		}
		var stale []string
		seen := make(map[string]struct{}, len(retained))
		for _, id := range retained {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			if _, alive := aliveIDs[id]; !alive {
				stale = append(stale, id)
			}
		}
		if len(stale) > 0 {
			if err := p.Forget(ctx, scope, stale); err != nil {
				r.log("recall: projection %s Forget: %v", p.Name(), err)
			}
		}
	}
	return nil
}

func projectReplaceOrAdd(ctx context.Context, p Projection, scope Scope, entries []Entry) error {
	if p == nil || len(entries) == 0 {
		return nil
	}
	if replacer, ok := p.(ProjectionReplacer); ok {
		return replacer.Replace(ctx, scope, entries)
	}
	return p.Project(ctx, scope, entries)
}

// listAll pages through retrieval.Index.List collecting every doc
// matching the filter. Used by the Reconciler — its caller bounds
// memory implicitly by the per-scope alive-entry count. Production
// deployments with >batchSize entries per scope should consider raising
// WithReconcileBatch (currently unexposed; revisit when needed).
func listAll(
	ctx context.Context,
	idx retrieval.Index,
	ns string,
	filter retrieval.Filter,
	batchSize int,
) ([]retrieval.Doc, error) {
	var out []retrieval.Doc
	var page string
	for {
		resp, err := idx.List(ctx, ns, retrieval.ListRequest{
			Filter:    filter,
			PageSize:  batchSize,
			PageToken: page,
		})
		if err != nil {
			return nil, err
		}
		if resp == nil {
			break
		}
		out = append(out, resp.Items...)
		if resp.NextPageToken == "" {
			break
		}
		page = resp.NextPageToken
	}
	return out, nil
}
