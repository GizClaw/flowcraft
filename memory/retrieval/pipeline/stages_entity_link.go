package pipeline

import "context"

// entityLinkLookupDefaultCap is the default [EntityLinkLookup.PerEntityCap]
// installed by [WithEntityLinkLane] when the caller does not pass
// [WithEntityLinkPerEntityCap]. 50 keeps the candidate set comfortably
// under [retrieval.DocGetter] round-trip overhead while still
// surfacing several batches of recency-grouped entries per hot
// entity. Reassess if profiling shows the lane is starved (raise) or
// dominating latency (lower).
const entityLinkLookupDefaultCap = 50

// EntityLinkResolver is implemented by callers that own an external
// entity → entry-id inverted index. The pipeline package never
// imports sdk/recall (would cycle), so the resolver is the entire
// surface the EntityLinkLookup stage and ModeEntityLink lane see.
//
// Implementations:
//
//   - sdk/recall provides the canonical implementation
//     (internalEntityLinkResolver) wrapping recall.EntityStore.
//     The Memory facade installs it automatically when
//     [recall.WithEntityStore] is enabled.
//   - Tests substitute hand-rolled doubles via [WithEntityLinkResolver]
//     to assert lane behaviour without spinning up a real EntityStore.
//
// Contract:
//
//   - Pure read. No side effects.
//   - The `namespace` argument is the SEARCH namespace (i.e. the
//     entry namespace), NOT the entity sibling namespace. The
//     resolver is responsible for deriving its own storage location
//     from `namespace` (recall.internalEntityLinkResolver uses
//     ScopeFromNamespace + EntityNamespaceFor for this).
//   - perEntityCap mirrors EntityStore.Lookup's semantics: 0 = no
//     cap; >0 returns at most that many ids per entity, drawn from
//     the most-recent end of the resolver's storage list.
//   - Order in the returned slice is preserved by downstream
//     stages — ResolveLinks SHOULD return ids by descending
//     usefulness (typically: per-entity recency-first, with
//     cross-entity deduplication maintaining the first-occurrence
//     position).
//
// Deprecated: use sdk/recall/pipeline.EntityLinkResolver. The retrieval-level
// entity-link resolver surface will be removed in v0.5.0.
type EntityLinkResolver interface {
	ResolveLinks(
		ctx context.Context,
		namespace string,
		entities []string,
		perEntityCap int,
	) ([]string, error)
}

// EntityLinkLookup expands [State.QueryEntities] into
// [State.CandidateEntityIDs] via an external [EntityLinkResolver].
//
// Reads:  State.QueryEntities, State.Namespace.
// Writes: State.CandidateEntityIDs.
//
// The stage is intentionally a thin shim — all the storage
// knowledge lives in the resolver. When Resolver is nil, the stage
// is a no-op so callers can install the option unconditionally and
// let the resolver decide whether the feature is wired (sdk/recall
// installs a nil resolver when the underlying index does not
// satisfy retrieval.DocGetter; the lane downstream sees an empty
// CandidateEntityIDs and behaves as if it were absent).
//
// Lookup errors are FATAL to the stage — they propagate to
// Pipeline.Run, which aborts. This matches the existing recall
// stages' policy (a failing Retrieve aborts the pipeline). Future
// "tolerant" mode tracked under the recall-degrade RFC.
//
// Deprecated: use sdk/recall/pipeline.EntityLinkLookup. The retrieval-level
// entity-link stage will be removed in v0.5.0.
type EntityLinkLookup struct {
	// Resolver is the entity → entry-id index implementation.
	// nil = stage is a no-op (still safe to keep in the pipeline).
	Resolver EntityLinkResolver

	// PerEntityCap caps the ids drawn from each entity. 0 = no
	// cap. Defaults to entityLinkLookupDefaultCap when both
	// PerEntityCap and the option-side default are zero.
	PerEntityCap int
}

// Name implements [Stage].
func (s EntityLinkLookup) Name() string { return "EntityLinkLookup" }

// Run implements [Stage]. See struct godoc for the contract.
func (s EntityLinkLookup) Run(ctx context.Context, st *State) error {
	if s.Resolver == nil || len(st.QueryEntities) == 0 {
		return nil
	}
	ids, err := s.Resolver.ResolveLinks(ctx, st.Namespace, st.QueryEntities, s.PerEntityCap)
	if err != nil {
		return err
	}
	st.CandidateEntityIDs = ids
	return nil
}
