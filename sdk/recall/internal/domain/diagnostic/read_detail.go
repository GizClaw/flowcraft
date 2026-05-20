package diagnostic

import "time"

// IntentDetail —— read/intent stage diagnostic.
type IntentDetail struct {
	RawQuery     string
	Entities     []string
	Kinds        []string
	Subject      string
	HasTimeRange bool
	GraphEnabled bool
	NERLatency   time.Duration
	LLMUsed      bool
}

func (IntentDetail) isStageDetail() {}

// PlanDetail —— read/plan stage diagnostic. ActivatedLenses captures
// which lenses the planner enabled and why.
type PlanDetail struct {
	ActivatedLenses []ActivatedLens
	TotalBudget     int
}

// ActivatedLens is one row in PlanDetail.ActivatedLenses.
type ActivatedLens struct {
	Lens        string
	Weight      float64
	Budget      int
	ActivatedBy string
}

func (PlanDetail) isStageDetail() {}

// FederationFanoutDetail —— read/federation_fanout stage (Phase D.5,
// v1 Partition generalisation). FastPath records the single-scope
// shortcut so dashboards can tell apart simple from federated reads.
type FederationFanoutDetail struct {
	SubScopes []SubScopeRun
	FastPath  bool
}

// SubScopeRun is one row in FederationFanoutDetail.SubScopes.
type SubScopeRun struct {
	Scope         string
	SourceResults int
	Materialized  int
	Latency       time.Duration
	Err           string
}

func (FederationFanoutDetail) isStageDetail() {}

// FederationMergeDetail —— read/federation_merge stage (Phase D.5).
type FederationMergeDetail struct {
	InputCount     int
	AfterDedup     int
	AfterTopK      int
	DroppedByDedup int
	Latency        time.Duration
}

func (FederationMergeDetail) isStageDetail() {}

// SourceFanoutDetail —— read/source_fanout stage (per-sub-scope when
// Federation is in use). Results captures each lens's contribution.
type SourceFanoutDetail struct {
	Results []SourceResult
}

// SourceResult is one row in SourceFanoutDetail.Results.
type SourceResult struct {
	Lens       string
	Candidates int
	Drops      []CandidateDrop
	Latency    time.Duration
	Err        string
}

func (SourceFanoutDetail) isStageDetail() {}

// FuseDetail —— read/fuse stage.
type FuseDetail struct {
	InputCount     int
	AfterDedup     int
	AfterTopK      int
	OutlierBoosted int
	DroppedByDedup int
	Latency        time.Duration
}

func (FuseDetail) isStageDetail() {}

// MaterializeDetail —— read/materialize stage.
type MaterializeDetail struct {
	Requested    int
	Returned     int
	Dropped      []DroppedFact
	StoreLatency time.Duration
}

func (MaterializeDetail) isStageDetail() {}

// TrustFilterDetail —— read/trust_filter stage (Phase D.2).
type TrustFilterDetail struct {
	MaxSensitivity string
	ActorID        string
	Removed        int
	Redacted       int
}

func (TrustFilterDetail) isStageDetail() {}

// RankDetail —— read/rank stage. Captures the post-fusion / pre-hits
// ranker's effect on the candidate pool.
type RankDetail struct {
	InputCount    int
	OutputCount   int
	FinalCap      int
	BoostsApplied int
	Latency       time.Duration
}

func (RankDetail) isStageDetail() {}

// BuildHitsDetail —— read/build_hits stage.
type BuildHitsDetail struct {
	Count int
}

func (BuildHitsDetail) isStageDetail() {}

// EvolutionAfterRecallDetail —— read/evolution_after_recall stage.
type EvolutionAfterRecallDetail struct {
	Repairs int
}

func (EvolutionAfterRecallDetail) isStageDetail() {}
