package diagnostic

import "time"

// IntentDetail —— read/intent stage diagnostic.
type IntentDetail struct {
	// QueryLen is the compiled query length; raw text is omitted from
	// diagnostics to avoid PII retention in telemetry after ForgetAll.
	QueryLen     int
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
// which lenses the planner enabled and why. ActivatedLens itself lives
// in shared.go (cross-detail vocabulary).
type PlanDetail struct {
	ActivatedLenses []ActivatedLens
	TotalBudget     int
}

func (PlanDetail) isStageDetail() {}

// FederationFanoutDetail —— read/federation_fanout stage (Phase D.5,
// v1 Partition generalisation). FastPath records the single-scope
// shortcut so dashboards can tell apart simple from federated reads.
type FederationFanoutDetail struct {
	SubScopes         []SubScopeRun
	FastPath          bool
	Sources           []SourceResult
	Drops             []CandidateDrop
	Fused             *[]CandidateSnapshot
	MaterializedItems *[]CandidateSnapshot
	FusedCandidates   int
	Materialized      int
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
	Items          *[]CandidateSnapshot
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
	Snapshots  *[]CandidateSnapshot
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
	Requested       int
	Returned        int
	Dropped         []DroppedFact
	StoreLatency    time.Duration
	RetiredFiltered int
}

func (MaterializeDetail) isStageDetail() {}

// TrustFilterDetail —— read/trust_filter stage (Phase D.2).
type TrustFilterDetail struct {
	MaxSensitivity string
	ActorID        string
	Removed        int
	Redacted       int
	Items          *[]CandidateSnapshot
}

func (TrustFilterDetail) isStageDetail() {}

// RankDetail —— read/rank stage. Captures the post-fusion / pre-hits
// ranker's effect on the candidate pool.
type RankDetail struct {
	InputCount             int
	OutputCount            int
	FinalCap               int
	BoostsApplied          int
	TimeDecayApplied       int
	SupersededDecayApplied int
	Latency                time.Duration
	Input                  *[]CandidateSnapshot
	Output                 *[]CandidateSnapshot
}

func (RankDetail) isStageDetail() {}

// BuildHitsDetail —— read/build_hits stage.
type BuildHitsDetail struct {
	Count                 int
	InputCount            int
	Reranked              int
	RerankErr             string
	Latency               time.Duration
	RerankLatency         time.Duration
	FinalSelectionLatency time.Duration
	Input                 *[]CandidateSnapshot
	RerankedHits          *[]CandidateSnapshot
	Hits                  *[]CandidateSnapshot
}

func (BuildHitsDetail) isStageDetail() {}

// EvolutionAfterRecallDetail —— read/evolution_after_recall stage.
type EvolutionAfterRecallDetail struct {
	Repairs int
}

func (EvolutionAfterRecallDetail) isStageDetail() {}

// PlanView is the planner output reconstructed from trace.Stages
// (intent + plan stage details).
type PlanView struct {
	SourceOrder    []string
	SourceBudgets  map[string]int
	TotalCap       int
	IntentText     string
	IntentSubject  string
	IntentEntities []string
}

// SourceView is one source lens contribution reconstructed from
// read-path stages (federation_fanout / source_fanout / plan).
type SourceView struct {
	Source    string
	Activated bool
	Budget    int
	Returned  int
	Truncated bool
	Latency   time.Duration
	Err       string
}

// ExtractPlan rebuilds planner output from intent + plan stage details.
func ExtractPlan(stages []StageDiagnostic) PlanView {
	var out PlanView
	out.SourceBudgets = map[string]int{}
	for _, st := range stages {
		switch st.Stage {
		case "intent":
			if d, ok := st.Detail.(IntentDetail); ok {
				_ = d.QueryLen // IntentText omitted from diagnostics (PII)
				out.IntentSubject = d.Subject
				out.IntentEntities = append([]string(nil), d.Entities...)
			}
		case "plan":
			if d, ok := st.Detail.(PlanDetail); ok {
				out.TotalCap = d.TotalBudget
				for _, l := range d.ActivatedLenses {
					out.SourceOrder = append(out.SourceOrder, l.Lens)
					out.SourceBudgets[l.Lens] = l.Budget
				}
			}
		}
	}
	return out
}

// ExtractSources lists source fan-out rows from federation_fanout or
// source_fanout, padded with non-activated lenses from the plan so
// dashboards see one row per registered lens.
func ExtractSources(stages []StageDiagnostic) []SourceView {
	plan := ExtractPlan(stages)
	var out []SourceView
	seen := map[string]bool{}
	appendSources := func(results []SourceResult) {
		for _, r := range results {
			seen[r.Lens] = true
			out = append(out, SourceView{
				Source:    r.Lens,
				Activated: true,
				Budget:    plan.SourceBudgets[r.Lens],
				Returned:  r.Candidates,
				Latency:   r.Latency,
				Err:       r.Err,
			})
		}
	}
	for _, st := range stages {
		switch st.Stage {
		case "federation_fanout":
			if d, ok := st.Detail.(FederationFanoutDetail); ok {
				appendSources(d.Sources)
			}
		case "source_fanout":
			if d, ok := st.Detail.(SourceFanoutDetail); ok {
				appendSources(d.Results)
			}
		}
	}
	for _, src := range plan.SourceOrder {
		if seen[src] {
			continue
		}
		out = append(out, SourceView{Source: src, Budget: plan.SourceBudgets[src]})
	}
	return out
}

// ExtractDrops collects read-path candidate drops from stage details.
func ExtractDrops(stages []StageDiagnostic) []CandidateDrop {
	var out []CandidateDrop
	for _, st := range stages {
		if st.Stage == "federation_fanout" {
			if d, ok := st.Detail.(FederationFanoutDetail); ok {
				out = append(out, d.Drops...)
			}
		}
	}
	return out
}

// ExtractFusedCandidates returns the fused pool size from stage details.
func ExtractFusedCandidates(stages []StageDiagnostic) int {
	for _, st := range stages {
		switch st.Stage {
		case "federation_fanout":
			if d, ok := st.Detail.(FederationFanoutDetail); ok {
				return d.FusedCandidates
			}
		case "fuse":
			if d, ok := st.Detail.(FuseDetail); ok {
				return d.AfterTopK
			}
		}
	}
	return 0
}

// ExtractMaterialized returns the materialized count from stage details.
func ExtractMaterialized(stages []StageDiagnostic) int {
	n := 0
	for _, st := range stages {
		switch st.Stage {
		case "federation_fanout":
			if d, ok := st.Detail.(FederationFanoutDetail); ok {
				n += d.Materialized
			}
		case "materialize":
			if d, ok := st.Detail.(MaterializeDetail); ok {
				n += d.Returned
			}
		case "rank":
			if d, ok := st.Detail.(RankDetail); ok && d.OutputCount > n {
				n = d.OutputCount
			}
		}
	}
	return n
}
