package retrieval

import "time"

// LaneKey is the canonical identifier of one retrieval lane that contributed
// to a search response.
//
// Backends MUST use one of the well-known constants below when populating
// SearchExecution.Lanes; opaque, backend-specific identifiers are allowed
// (e.g. "pgvector.hybrid"), but the leading segment SHOULD describe the
// retrieval modality so cross-backend dashboards remain meaningful.
type LaneKey string

// Well-known lane keys. Pipeline lanes (configured via
// pipeline.MultiRetrieve / Retrieve) are surfaced under these names so that
// downstream consumers can render an explanation regardless of which backend
// (memory, postgres, native hybrid, ...) actually produced the hits.
const (
	LaneBM25       LaneKey = "bm25"
	LaneVector     LaneKey = "vector"
	LaneSparse     LaneKey = "sparse"
	LaneEntity     LaneKey = "entity"
	LaneEntityLink LaneKey = "entity_link"
	LaneHybrid     LaneKey = "hybrid"
	LaneFusion     LaneKey = "fusion"
	LaneRerank     LaneKey = "rerank"
	LanePostFilter LaneKey = "postfilter"
)

// LaneResult describes the contribution of a single retrieval lane to a
// SearchExecution. Hits is the lane's pre-fusion ranking (already trimmed by
// the lane's TopK).
//
// Hits MUST NOT be aliased with the final SearchResponse.Hits slice; backends
// SHOULD copy to keep the explanation immutable from the caller's POV.
type LaneResult struct {
	Key      LaneKey
	Hits     []Hit
	Took     time.Duration
	Filtered int64
	Note     string
}

// StageResult is one entry in SearchExecution.Stages, describing a stage
// (recall lane, fusion, rerank, post-filter, ...) the backend ran.
type StageResult struct {
	Name    string
	Took    time.Duration
	Err     string
	HitsIn  int
	HitsOut int
}

// SearchExecution is the structured explanation of how a SearchResponse was
// produced. It is populated when SearchRequest.Debug.IncludeLanes /
// IncludeStages are set, and is otherwise nil.
//
// Stability: this is the SDK's public, backend-neutral debugging surface.
// Adding fields is allowed; existing fields are subject to the SDK's
// backwards-compatibility guarantee.
type SearchExecution struct {
	Lanes  []LaneResult
	Stages []StageResult
}

// SearchDebug controls how much execution detail a backend should attach to
// SearchResponse.Execution. Zero value disables all debugging output and is
// the documented default.
type SearchDebug struct {
	IncludeLanes  bool
	IncludeStages bool
}
