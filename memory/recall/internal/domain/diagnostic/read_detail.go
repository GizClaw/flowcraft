package diagnostic

import "time"

// QueryUnderstandDetail —— read/query_understand stage diagnostic.
type QueryUnderstandDetail struct {
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

func (QueryUnderstandDetail) isStageDetail() {}

// PlanDetail —— read/plan stage diagnostic. ActivatedLenses captures
// which lenses the planner enabled and why. ActivatedLens itself lives
// in shared.go (cross-detail vocabulary).
type PlanDetail struct {
	ActivatedLenses []ActivatedLens
	TotalBudget     int
	TaskIntents     []string
}

func (PlanDetail) isStageDetail() {}

// CandidateFanoutDetail records source fanout across all effective scopes.
type CandidateFanoutDetail struct {
	SubScopes []SubScopeRun
	Sources   []SourceResult
}

func (CandidateFanoutDetail) isStageDetail() {}

// CandidateMergeAndMaterializeDetail records source-result merging,
// materialization, and cross-scope dedupe as one candidate pool construction
// step.
type CandidateMergeAndMaterializeDetail struct {
	SubScopes             []SubScopeRun
	InputCount            int
	CandidateCount        int
	MaterializedCount     int
	OutputCount           int
	DroppedByDedup        int
	Drops                 []CandidateDrop
	CandidateSnapshots    *[]CandidateSnapshot
	MaterializedSnapshots *[]CandidateSnapshot
	Output                *[]CandidateSnapshot
	Latency               time.Duration
}

func (CandidateMergeAndMaterializeDetail) isStageDetail() {}

// SubScopeRun is one per-scope row in fanout/merge diagnostics.
type SubScopeRun struct {
	Scope         string
	SourceResults int
	Materialized  int
	Latency       time.Duration
	Err           string
}

// SourceResult is one row in CandidateFanoutDetail.Sources.
type SourceResult struct {
	Lens          string
	Candidates    int
	QueryVariants int
	Snapshots     *[]CandidateSnapshot
	Drops         []CandidateDrop
	Latency       time.Duration
	Err           string
}

// CandidateExpansionDetail captures bounded structural candidate additions
// before policy/rank. Added candidates are score-neutral inputs; rank and
// context packing still decide whether they surface.
type CandidateExpansionDetail struct {
	InputCount       int
	OutputCount      int
	Scanned          int
	Added            int
	TaskIntents      []string
	AddedFactIDs     []string
	Suggested        int
	SuggestedByTask  map[string]int
	SuggestedFactIDs []string
	Items            *[]CandidateSnapshot
	Err              string
}

func (CandidateExpansionDetail) isStageDetail() {}

// ObservationRecallDetail captures raw observation candidates added when the
// assertion lanes miss or under-cover the query.
type ObservationRecallDetail struct {
	InputCount          int
	OutputCount         int
	ScannedObservations int
	AddedObservations   int
	AddedObservationIDs []string
	Latency             time.Duration
	Err                 string
	Items               *[]CandidateSnapshot
}

func (ObservationRecallDetail) isStageDetail() {}

// LinkExpansionDetail captures bounded canonical graph expansion before
// policy/rank. It records both linked assertion additions and observation
// evidence attached to already-materialized assertions.
type LinkExpansionDetail struct {
	InputCount        int
	OutputCount       int
	ScannedLinks      int
	AddedFacts        int
	AddedEvidenceRefs int
	AddedFactIDs      []string
	Latency           time.Duration
	Err               string
	Items             *[]CandidateSnapshot
}

func (LinkExpansionDetail) isStageDetail() {}

// PolicyFilterDetail —— read/policy_filter stage.
type PolicyFilterDetail struct {
	MaxSensitivity string
	ActorID        string
	Removed        int
	Redacted       int
	Items          *[]CandidateSnapshot
}

func (PolicyFilterDetail) isStageDetail() {}

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

// ContextPackDetail —— read/context_pack stage.
type ContextPackDetail struct {
	Count                 int
	InputCount            int
	Reranked              int
	RerankErr             string
	Latency               time.Duration
	RerankLatency         time.Duration
	ContextPackingLatency time.Duration
	CoverageBundles       []CoverageBundle
	Input                 *[]CandidateSnapshot
	RerankedHits          *[]CandidateSnapshot
	Hits                  *[]CandidateSnapshot
}

func (ContextPackDetail) isStageDetail() {}

// CoverageBundle records a deterministic context-pack rescue that keeps
// structurally related evidence together for answerability.
type CoverageBundle struct {
	SeedFactID      string
	RescuedFactIDs  []string
	ReplacedFactIDs []string
	Reason          string
}

// BuildGroundedHitsDetail —— read/build_grounded_hits stage.
type BuildGroundedHitsDetail struct {
	Count      int
	InputCount int
	Latency    time.Duration
	Input      *[]CandidateSnapshot
	Hits       *[]CandidateSnapshot
}

func (BuildGroundedHitsDetail) isStageDetail() {}

// EvolutionAfterRecallDetail —— read/evolution_after_recall stage.
type EvolutionAfterRecallDetail struct {
	Repairs int
}

func (EvolutionAfterRecallDetail) isStageDetail() {}

// PlanView is the planner output reconstructed from trace.Stages.
type PlanView struct {
	SourceOrder    []string
	SourceBudgets  map[string]int
	TotalCap       int
	IntentText     string
	IntentSubject  string
	IntentEntities []string
}

// SourceView is one source lens contribution reconstructed from read-path
// stages.
type SourceView struct {
	Source    string
	Activated bool
	Budget    int
	Returned  int
	Truncated bool
	Latency   time.Duration
	Err       string
}

// ExtractPlan rebuilds planner output from query-understanding + plan stage
// details.
func ExtractPlan(stages []StageDiagnostic) PlanView {
	var out PlanView
	out.SourceBudgets = map[string]int{}
	for _, st := range stages {
		switch st.Stage {
		case "query_understand":
			if d, ok := st.Detail.(QueryUnderstandDetail); ok {
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

// ExtractSources lists source fanout rows, padded with non-activated lenses
// from the plan so dashboards see one row per registered lens.
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
		case "candidate_fanout":
			if d, ok := st.Detail.(CandidateFanoutDetail); ok {
				appendSources(d.Sources)
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
		switch st.Stage {
		case "candidate_merge_and_materialize":
			if d, ok := st.Detail.(CandidateMergeAndMaterializeDetail); ok {
				out = append(out, d.Drops...)
			}
		}
	}
	return out
}

// ExtractCandidateCount returns the merged candidate pool size from stage
// details.
func ExtractCandidateCount(stages []StageDiagnostic) int {
	for _, st := range stages {
		switch st.Stage {
		case "candidate_merge_and_materialize":
			if d, ok := st.Detail.(CandidateMergeAndMaterializeDetail); ok {
				return d.CandidateCount
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
		case "candidate_merge_and_materialize":
			if d, ok := st.Detail.(CandidateMergeAndMaterializeDetail); ok {
				n += d.MaterializedCount
			}
		case "rank":
			if d, ok := st.Detail.(RankDetail); ok && d.OutputCount > n {
				n = d.OutputCount
			}
		}
	}
	return n
}
