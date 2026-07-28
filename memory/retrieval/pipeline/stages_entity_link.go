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
// imports recall (would cycle), so the resolver is the entire surface the
// EntityLinkLookup stage and ModeEntityLink lane see. Tests substitute
// hand-rolled doubles via [WithEntityLinkResolver].
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
// Deprecated: use memory/recall.
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
// let the resolver decide whether the feature is wired.
//
// Lookup errors are FATAL to the stage — they propagate to
// Pipeline.Run, which aborts. This matches the existing recall
// stages' policy (a failing Retrieve aborts the pipeline). Future
// "tolerant" mode tracked under the recall-degrade RFC.
//
// Deprecated: use memory/recall.
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
