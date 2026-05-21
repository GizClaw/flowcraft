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
