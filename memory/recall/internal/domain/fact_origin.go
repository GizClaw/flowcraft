package domain

// FactOriginKind classifies the durable work item that produced a fact.
// Used for retry idempotency via FindByOriginRequestID (see
// recall-v2-async-semantic-write.md §3.3).
type FactOriginKind string

const (
	// OriginKindUnspecified is the zero value — pre-Origin facts and
	// synchronous direct writes carry no origin.
	OriginKindUnspecified FactOriginKind = ""
	// OriginKindEpisode marks raw conversation episodes captured by
	// the sync episode lane.
	OriginKindEpisode FactOriginKind = "episode"
	// OriginKindSemanticDerivation marks semantic facts produced by
	// the async worker from a prior episode.
	OriginKindSemanticDerivation FactOriginKind = "semantic_derivation"
)

// FactOrigin is the idempotency identifier for facts that participate
// in a durable workflow. RequestID is the work-item key; EpisodeFactIDs
// links semantic derivations back to their source episodes.
//
// Origin answers "which durable work item produced these facts" and is
// distinct from:
//   - EvidenceRefs (audit / citation provenance)
//   - Revision.SourceFactID (canonical Fork/Contest/Supersede lineage)
type FactOrigin struct {
	RequestID      string
	Kind           FactOriginKind
	EpisodeFactIDs []string
}

// IsZero reports whether the origin is unset (zero value).
func (o FactOrigin) IsZero() bool {
	return o.RequestID == "" && o.Kind == OriginKindUnspecified && len(o.EpisodeFactIDs) == 0
}
