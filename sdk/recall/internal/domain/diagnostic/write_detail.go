package diagnostic

import "time"

// ValidateDetail —— write/validate stage (SaveRequest fields / Scope /
// Trust validation).
type ValidateDetail struct {
	InputTurns   int
	Rejected     int
	RejectReason string
}

func (ValidateDetail) isStageDetail() {}

// IngestDetail —— write/ingest stage diagnostic. Carries the
// StructurizerCoverage canonical type so the ingest pipeline does
// not need a second tally model.
type IngestDetail struct {
	InputTurns           int
	ExtractedFacts       int
	DroppedByPolicy      int
	DroppedByValidation  int
	DroppedByDedup       int
	StructurizerCoverage StructurizerCoverage
	ExtractorLatency     time.Duration
	StructurizerLatency  time.Duration
	// RecentMessagesProvided / AnchorsProvided / TierApplied are
	// Phase D.7 / D.3 fields wired through SaveRequest. Zero values
	// here today.
	RecentMessagesProvided int
	AnchorsProvided        int
	TierApplied            string
	Dropped                []DroppedFact
	KnownEntitiesSeen      int
}

func (IngestDetail) isStageDetail() {}

// ResolveDetail —— write/resolve stage. Supersede / fork / contest
// counters break the resolver's RevisionKind decisions out for
// telemetry.
type ResolveDetail struct {
	Candidates int
	Appended   int
	Closed     int
	Superseded int
	Forked     int
	Merged     int
	Contested  int
}

func (ResolveDetail) isStageDetail() {}

// AppendDetail —— write/append stage.
type AppendDetail struct {
	Facts        int
	StoreLatency time.Duration
	RetryCount   int
}

func (AppendDetail) isStageDetail() {}

// ValidityCloseDetail —— write/validity_close stage (ValidTo auto-
// close for facts superseded by the current Save).
type ValidityCloseDetail struct {
	ClosedFacts  int
	StoreLatency time.Duration
}

func (ValidityCloseDetail) isStageDetail() {}

// EvidenceMirrorDetail —— write/evidence_mirror stage (mirror
// EvidenceRefs into the secondary lookup store).
type EvidenceMirrorDetail struct {
	EventsRecorded int
	Latency        time.Duration
}

func (EvidenceMirrorDetail) isStageDetail() {}

// ProjectDetail —— write/project_required AND write/project_optional
// share this Detail; Consistency disambiguates ("required" vs
// "optional"). Results lists per-projection outcomes.
type ProjectDetail struct {
	Consistency string
	Results     []ProjectionResult
}

// ProjectionResult is one row in ProjectDetail.Results.
type ProjectionResult struct {
	Name    string
	Applied int
	Dropped int
	Latency time.Duration
	Err     string
}

func (ProjectDetail) isStageDetail() {}

// EvolutionAfterSaveDetail —— write/evolution_after_save stage.
type EvolutionAfterSaveDetail struct {
	Repairs        int
	Decays         int
	Consolidations int
	// ReinforcedRefs is wired in Phase D.4.
	ReinforcedRefs int
}

func (EvolutionAfterSaveDetail) isStageDetail() {}

// ForgetAllDetail —— forget_all stage (Phase D.8 C9; GDPR Art.17 /
// CCPA 1798.105 compliant scope-level retirement).
//
// Mode is "soft" (Closed=true, store retained) or "hard" (physical
// delete from canonical store + projections + evidence). For Soft
// mode EvidenceCleared stays 0 — evidence is preserved for audit so
// Memory.History can still rebuild the supersede chain.
type ForgetAllDetail struct {
	ScopeKey           string
	Mode               string
	Deleted            int
	ProjectionsCleared int
	EvidenceCleared    int
	AsyncJobsCancelled int
	AsyncJobCancelErr  string
	Latency            time.Duration
}

func (ForgetAllDetail) isStageDetail() {}

// ExpireRetiredDetail —— forget_all stage (TTL filter variant).
//
// Emitted by the forget_all stage when invoked through
// Memory.ExpireRetired (D5 2026-05-21). The stage reuses the forget
// pipeline runner with State.Filter set to ExpiresBefore; the
// resulting telemetry carries the TTL boundary, the matched/deleted
// counts, and per-projection forget counts so operators can audit a
// scheduled retention sweep without inspecting the canonical store.
type ExpireRetiredDetail struct {
	ScopeKey           string
	ExpiresBefore      time.Time
	Scanned            int
	Deleted            int
	ProjectionsHit     int
	AsyncJobsCancelled int
	AsyncJobCancelErr  string
	Latency            time.Duration
}

func (ExpireRetiredDetail) isStageDetail() {}

// FeedbackDetail —— feedback/apply_feedback stage (Cluster A 2026-05-21).
//
// Reinforce / Penalize now route through the feedback pipeline so the
// fact UpdateFeedback write + single-fact reproject (Cluster D) emit a
// single observable record. Either delta MAY be zero (one-channel
// updates are normal).
type FeedbackDetail struct {
	FactID             string
	ReinforcementDelta float64
	PenaltyDelta       float64
	Latency            time.Duration
}

func (FeedbackDetail) isStageDetail() {}

// RevisionDetail —— revision/{lookup_source,attach,save} stages
// (Cluster A 2026-05-21).
//
// Fork / Contest run as a three-stage pipeline; each stage emits its
// own RevisionDetail describing the action taken. Kind disambiguates
// the revision flavour, Stage names the specific step, SourceFactID
// is the prior fact the revision anchors on, and CreatedFactID is the
// new fact id once the save stage commits.
type RevisionDetail struct {
	Kind          string
	Stage         string
	SourceFactID  string
	CreatedFactID string
	Latency       time.Duration
}

func (RevisionDetail) isStageDetail() {}

// BuildEpisodeDetail —— write/build_episode stage (Phase F.1a). The
// sync episode lane runs this stage to translate SaveRequest.Turns
// into one or more KindEpisode facts before append. AsyncRequestID
// is the durable work-item key shared across the sync lane and the
// eventual async semantic worker (see
// recall-v2-async-semantic-write.md §7.2).
type BuildEpisodeDetail struct {
	Turns          int
	EpisodeFacts   int
	AsyncRequestID string
}

func (BuildEpisodeDetail) isStageDetail() {}

// ProjectEpisodeEvidenceDetail —— write/project_episode_evidence stage
// (Phase F.1a). Records the kind-filtered fanout that mirrors raw
// episode evidence into the evidence projection (the only required
// projection that accepts KindEpisode).
type ProjectEpisodeEvidenceDetail struct {
	AsyncRequestID string
	EpisodeFacts   int
	Latency        time.Duration
}

func (ProjectEpisodeEvidenceDetail) isStageDetail() {}

// EnqueueSemanticDetail —— write/write_semantic_outbox stage (Phase
// F.1a). Captures the local-durable outbox boundary: the synchronous
// Save returns only after this enqueue completes.
type EnqueueSemanticDetail struct {
	AsyncRequestID string
	EpisodeFactIDs []string
	Latency        time.Duration
}

func (EnqueueSemanticDetail) isStageDetail() {}

// OriginStampDetail —— write/origin_stamp stage (Phase F.1b). Records
// how many resolved facts received SemanticDerivation origin metadata
// before append in the async worker lane.
type OriginStampDetail struct {
	AsyncRequestID string
	Facts          int
}

func (OriginStampDetail) isStageDetail() {}

// ExtractSaveDropped returns write-path compiler drops from ingest detail.
func ExtractSaveDropped(stages []StageDiagnostic) []DroppedFact {
	for _, st := range stages {
		if st.Stage == "ingest" {
			if d, ok := st.Detail.(IngestDetail); ok {
				return append([]DroppedFact(nil), d.Dropped...)
			}
		}
	}
	return nil
}

// ExtractStructurizerCoverage reads ingest stage coverage tallies.
func ExtractStructurizerCoverage(stages []StageDiagnostic) StructurizerCoverage {
	for _, st := range stages {
		if st.Stage == "ingest" {
			if d, ok := st.Detail.(IngestDetail); ok {
				return d.StructurizerCoverage
			}
		}
	}
	return StructurizerCoverage{}
}

// ExtractKnownEntitiesSeen returns entity snapshot count from ingest.
func ExtractKnownEntitiesSeen(stages []StageDiagnostic) int {
	for _, st := range stages {
		if st.Stage == "ingest" {
			if d, ok := st.Detail.(IngestDetail); ok {
				return d.KnownEntitiesSeen
			}
		}
	}
	return 0
}
