package diagnostic

import (
	"testing"
	"time"
)

// TestStructurizerCoverage_Add pins the per-field summation: callers
// fold per-fact deltas into a single per-Save total, so every counter
// must accumulate independently.
func TestStructurizerCoverage_Add(t *testing.T) {
	c := StructurizerCoverage{TotalFactsSeen: 1, KindFilled: 1}
	c.Add(StructurizerCoverage{
		TotalFactsSeen:      2,
		KindFilled:          1,
		EntitiesFilled:      3,
		SubjectFilled:       4,
		ValidFromHintFilled: 5,
	})
	want := StructurizerCoverage{
		TotalFactsSeen:      3,
		KindFilled:          2,
		EntitiesFilled:      3,
		SubjectFilled:       4,
		ValidFromHintFilled: 5,
	}
	if c != want {
		t.Errorf("Add = %+v, want %+v", c, want)
	}
}

func TestHasStage(t *testing.T) {
	stages := []StageDiagnostic{{Stage: "intent"}, {Stage: "plan"}}
	if !HasStage(stages, "plan") {
		t.Error("HasStage(plan) → true")
	}
	if HasStage(stages, "missing") {
		t.Error("HasStage(missing) → false")
	}
	if HasStage(nil, "anything") {
		t.Error("HasStage(nil) → false")
	}
}

// readStages is a minimal happy-path read trace exercising every
// branch the Extract* helpers care about.
func readStages() []StageDiagnostic {
	return []StageDiagnostic{
		{
			Stage: "intent",
			Detail: IntentDetail{
				QueryLen: 22,
				Subject:  "alice",
				Entities: []string{"alice"},
			},
		},
		{
			Stage: "plan",
			Detail: PlanDetail{
				TotalBudget: 12,
				ActivatedLenses: []ActivatedLens{
					{Lens: "retrieval", Weight: 1.0, Budget: 5, ActivatedBy: "default"},
					{Lens: "timeline", Weight: 0.5, Budget: 7, ActivatedBy: "intent"},
				},
			},
		},
		{
			Stage: "federation_fanout",
			Detail: FederationFanoutDetail{
				Sources: []SourceResult{
					{Lens: "retrieval", Candidates: 3, Latency: 5 * time.Millisecond},
				},
				Drops: []CandidateDrop{
					{Stage: "fusion", Reason: DropTotalCap, FactID: "f-drop"},
				},
				FusedCandidates: 4,
				Materialized:    2,
			},
		},
		{
			Stage:  "materialize",
			Detail: MaterializeDetail{Requested: 3, Returned: 2},
		},
		{
			Stage:  "rank",
			Detail: RankDetail{InputCount: 2, OutputCount: 2},
		},
	}
}

func TestExtractPlan(t *testing.T) {
	got := ExtractPlan(readStages())
	if got.IntentText != "" {
		t.Errorf("IntentText must be redacted from diagnostics, got %q", got.IntentText)
	}
	if got.IntentSubject != "alice" {
		t.Errorf("IntentSubject = %q", got.IntentSubject)
	}
	if got.TotalCap != 12 {
		t.Errorf("TotalCap = %d", got.TotalCap)
	}
	if len(got.SourceOrder) != 2 || got.SourceOrder[0] != "retrieval" {
		t.Errorf("SourceOrder = %v", got.SourceOrder)
	}
	if got.SourceBudgets["timeline"] != 7 {
		t.Errorf("SourceBudgets[timeline] = %d", got.SourceBudgets["timeline"])
	}
	// IntentEntities must be a copy: mutating the result must not
	// poison the next ExtractPlan() call on the same trace.
	stages := readStages()
	v1 := ExtractPlan(stages)
	v1.IntentEntities[0] = "MUT"
	v2 := ExtractPlan(stages)
	if v2.IntentEntities[0] != "alice" {
		t.Errorf("IntentEntities aliased: %v", v2.IntentEntities)
	}
}

// TestExtractSources_PadsNonActivated pins the second half of
// ExtractSources: plan-activated lenses that didn't show up in any
// fanout detail must still get a placeholder row so dashboards see
// one row per registered lens.
func TestExtractSources_PadsNonActivated(t *testing.T) {
	got := ExtractSources(readStages())
	if len(got) != 2 {
		t.Fatalf("want 2 source rows (retrieval activated + timeline padded), got %d: %+v", len(got), got)
	}
	if got[0].Source != "retrieval" || !got[0].Activated || got[0].Budget != 5 || got[0].Returned != 3 {
		t.Errorf("retrieval row = %+v", got[0])
	}
	if got[1].Source != "timeline" || got[1].Activated || got[1].Budget != 7 {
		t.Errorf("timeline padding row = %+v", got[1])
	}
}

func TestExtractDrops(t *testing.T) {
	drops := ExtractDrops(readStages())
	if len(drops) != 1 || drops[0].FactID != "f-drop" {
		t.Errorf("drops = %+v", drops)
	}
}

func TestExtractFusedCandidates(t *testing.T) {
	if n := ExtractFusedCandidates(readStages()); n != 4 {
		t.Errorf("federation_fanout fused = %d, want 4", n)
	}
	// fuse stage falls through when federation_fanout is absent
	stages := []StageDiagnostic{
		{Stage: "fuse", Detail: FuseDetail{AfterTopK: 9}},
	}
	if n := ExtractFusedCandidates(stages); n != 9 {
		t.Errorf("fuse fallback = %d, want 9", n)
	}
	if n := ExtractFusedCandidates(nil); n != 0 {
		t.Errorf("empty trace = %d, want 0", n)
	}
}

func TestExtractMaterialized(t *testing.T) {
	if n := ExtractMaterialized(readStages()); n != 4 {
		// federation_fanout (2) + materialize (2) → 4; rank == 2 < 4 so does not override.
		t.Errorf("want 4 (sum across federation+materialize), got %d", n)
	}
	// When rank.OutputCount exceeds the running total, it wins
	// (the rank stage describes a richer post-fusion pool).
	stages := []StageDiagnostic{
		{Stage: "materialize", Detail: MaterializeDetail{Returned: 1}},
		{Stage: "rank", Detail: RankDetail{OutputCount: 7}},
	}
	if n := ExtractMaterialized(stages); n != 7 {
		t.Errorf("rank override = %d, want 7", n)
	}
}

func TestExtractSaveDropped(t *testing.T) {
	stages := []StageDiagnostic{
		{
			Stage: "ingest",
			Detail: IngestDetail{
				Dropped: []DroppedFact{{Reason: "duplicate"}},
			},
		},
	}
	got := ExtractSaveDropped(stages)
	if len(got) != 1 || got[0].Reason != "duplicate" {
		t.Errorf("ExtractSaveDropped = %+v", got)
	}
	// Returned slice must be a copy.
	got[0].Reason = "MUT"
	if stages[0].Detail.(IngestDetail).Dropped[0].Reason != "duplicate" {
		t.Errorf("ExtractSaveDropped must not alias source slice")
	}
	if ExtractSaveDropped(nil) != nil {
		t.Errorf("nil trace returns nil drops")
	}
}

func TestExtractStructurizerCoverage(t *testing.T) {
	cov := StructurizerCoverage{TotalFactsSeen: 7, KindFilled: 5}
	stages := []StageDiagnostic{
		{Stage: "ingest", Detail: IngestDetail{StructurizerCoverage: cov}},
	}
	if got := ExtractStructurizerCoverage(stages); got != cov {
		t.Errorf("coverage = %+v, want %+v", got, cov)
	}
	if got := ExtractStructurizerCoverage(nil); got != (StructurizerCoverage{}) {
		t.Errorf("missing ingest → zero coverage, got %+v", got)
	}
}

func TestExtractKnownEntitiesSeen(t *testing.T) {
	stages := []StageDiagnostic{
		{Stage: "ingest", Detail: IngestDetail{KnownEntitiesSeen: 9}},
	}
	if n := ExtractKnownEntitiesSeen(stages); n != 9 {
		t.Errorf("known entities = %d, want 9", n)
	}
	if n := ExtractKnownEntitiesSeen(nil); n != 0 {
		t.Errorf("missing ingest = %d, want 0", n)
	}
}
