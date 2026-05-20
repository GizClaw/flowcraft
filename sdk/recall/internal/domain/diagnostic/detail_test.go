package diagnostic_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

// Compile-time assertions: every documented stage Detail satisfies
// StageDetail. Listed in pipeline-diagnostics.md §2.2 order — 8
// write + 11 read + 2 rebuild = 21 distinct Detail types (the doc
// mentions "20+" because the read/source_fanout nest carries the
// per-lens SourceResult tally without needing its own StageDetail).
var (
	_ diagnostic.StageDetail = diagnostic.ValidateDetail{}
	_ diagnostic.StageDetail = diagnostic.IngestDetail{}
	_ diagnostic.StageDetail = diagnostic.ResolveDetail{}
	_ diagnostic.StageDetail = diagnostic.AppendDetail{}
	_ diagnostic.StageDetail = diagnostic.ValidityCloseDetail{}
	_ diagnostic.StageDetail = diagnostic.EvidenceMirrorDetail{}
	_ diagnostic.StageDetail = diagnostic.ProjectDetail{}
	_ diagnostic.StageDetail = diagnostic.EvolutionAfterSaveDetail{}

	_ diagnostic.StageDetail = diagnostic.IntentDetail{}
	_ diagnostic.StageDetail = diagnostic.PlanDetail{}
	_ diagnostic.StageDetail = diagnostic.FederationFanoutDetail{}
	_ diagnostic.StageDetail = diagnostic.FederationMergeDetail{}
	_ diagnostic.StageDetail = diagnostic.SourceFanoutDetail{}
	_ diagnostic.StageDetail = diagnostic.FuseDetail{}
	_ diagnostic.StageDetail = diagnostic.MaterializeDetail{}
	_ diagnostic.StageDetail = diagnostic.TrustFilterDetail{}
	_ diagnostic.StageDetail = diagnostic.RankDetail{}
	_ diagnostic.StageDetail = diagnostic.BuildHitsDetail{}
	_ diagnostic.StageDetail = diagnostic.EvolutionAfterRecallDetail{}

	_ diagnostic.StageDetail = diagnostic.ScanDetail{}
	_ diagnostic.StageDetail = diagnostic.RebuildProjectionDetail{}
)

// TestDetail_RoundTrip pins JSON byte-stability for every Detail
// type. Each case constructs a representative non-zero value,
// marshals it, unmarshals into a fresh value of the SAME concrete
// type, then re-marshals and asserts bytes match. Polymorphic
// unmarshal (StageDetail interface) is intentionally NOT tested
// here — the pipeline framework owns that in Phase B; this test
// only locks the per-Detail JSON contract in place.
func TestDetail_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   diagnostic.StageDetail
		out  diagnostic.StageDetail
	}{
		{"validate", diagnostic.ValidateDetail{InputTurns: 3, Rejected: 1, RejectReason: "scope"}, &diagnostic.ValidateDetail{}},
		{"ingest", diagnostic.IngestDetail{
			InputTurns:           2,
			ExtractedFacts:       4,
			DroppedByPolicy:      1,
			DroppedByValidation:  0,
			DroppedByDedup:       1,
			StructurizerCoverage: diagnostic.StructurizerCoverage{TotalFactsSeen: 4, KindFilled: 4, EntitiesFilled: 3, SubjectFilled: 4, ValidFromHintFilled: 2},
			ExtractorLatency:     12 * time.Millisecond,
			StructurizerLatency:  3 * time.Millisecond,
			TierApplied:          "default",
		}, &diagnostic.IngestDetail{}},
		{"resolve", diagnostic.ResolveDetail{Candidates: 4, Appended: 3, Closed: 1, Superseded: 1, Forked: 0, Merged: 1, Contested: 0}, &diagnostic.ResolveDetail{}},
		{"append", diagnostic.AppendDetail{Facts: 2, StoreLatency: 5 * time.Millisecond, RetryCount: 0}, &diagnostic.AppendDetail{}},
		{"validity_close", diagnostic.ValidityCloseDetail{ClosedFacts: 1, StoreLatency: 4 * time.Millisecond}, &diagnostic.ValidityCloseDetail{}},
		{"evidence_mirror", diagnostic.EvidenceMirrorDetail{EventsRecorded: 5, Latency: 2 * time.Millisecond}, &diagnostic.EvidenceMirrorDetail{}},
		{"project", diagnostic.ProjectDetail{
			Consistency: "required",
			Results: []diagnostic.ProjectionResult{
				{Name: "retrieval", Applied: 2, Dropped: 0, Latency: 1 * time.Millisecond, Err: ""},
				{Name: "entity", Applied: 2, Dropped: 0, Latency: 1 * time.Millisecond, Err: ""},
			},
		}, &diagnostic.ProjectDetail{}},
		{"evolution_after_save", diagnostic.EvolutionAfterSaveDetail{Repairs: 0, Decays: 0, Consolidations: 1, ReinforcedRefs: 3}, &diagnostic.EvolutionAfterSaveDetail{}},

		{"intent", diagnostic.IntentDetail{
			RawQuery:     "where did alice go?",
			Entities:     []string{"alice"},
			Kinds:        []string{"event", "state"},
			Subject:      "alice",
			HasTimeRange: false,
			GraphEnabled: false,
			NERLatency:   1 * time.Millisecond,
			LLMUsed:      false,
		}, &diagnostic.IntentDetail{}},
		{"plan", diagnostic.PlanDetail{
			ActivatedLenses: []diagnostic.ActivatedLens{{Lens: "retrieval", Weight: 1.0, Budget: 20, ActivatedBy: "default"}},
			TotalBudget:     20,
		}, &diagnostic.PlanDetail{}},
		{"federation_fanout", diagnostic.FederationFanoutDetail{
			SubScopes: []diagnostic.SubScopeRun{{Scope: "rt:u1", SourceResults: 6, Materialized: 5, Latency: 7 * time.Millisecond}},
			FastPath:  true,
		}, &diagnostic.FederationFanoutDetail{}},
		{"federation_merge", diagnostic.FederationMergeDetail{InputCount: 12, AfterDedup: 10, AfterTopK: 5, DroppedByDedup: 2, Latency: 2 * time.Millisecond}, &diagnostic.FederationMergeDetail{}},
		{"source_fanout", diagnostic.SourceFanoutDetail{
			Results: []diagnostic.SourceResult{{Lens: "retrieval", Candidates: 8, Latency: 3 * time.Millisecond}},
		}, &diagnostic.SourceFanoutDetail{}},
		{"fuse", diagnostic.FuseDetail{InputCount: 30, AfterDedup: 25, AfterTopK: 10, OutlierBoosted: 1, DroppedByDedup: 5, Latency: 2 * time.Millisecond}, &diagnostic.FuseDetail{}},
		{"materialize", diagnostic.MaterializeDetail{Requested: 10, Returned: 9, StoreLatency: 4 * time.Millisecond}, &diagnostic.MaterializeDetail{}},
		{"trust_filter", diagnostic.TrustFilterDetail{MaxSensitivity: "private", ActorID: "agent-a", Removed: 2, Redacted: 1}, &diagnostic.TrustFilterDetail{}},
		{"rank", diagnostic.RankDetail{InputCount: 9, OutputCount: 9, FinalCap: 10, BoostsApplied: 2, Latency: 1 * time.Millisecond}, &diagnostic.RankDetail{}},
		{"build_hits", diagnostic.BuildHitsDetail{Count: 9}, &diagnostic.BuildHitsDetail{}},
		{"evolution_after_recall", diagnostic.EvolutionAfterRecallDetail{Repairs: 1}, &diagnostic.EvolutionAfterRecallDetail{}},

		{"scan", diagnostic.ScanDetail{ScopeKey: "rt:u1", Total: 200, AfterValidity: 180, Latency: 30 * time.Millisecond}, &diagnostic.ScanDetail{}},
		{"rebuild_projection", diagnostic.RebuildProjectionDetail{ProjectionName: "retrieval", Applied: 200, Dropped: 0, PriorEntries: 200, DriftDetected: false, Latency: 50 * time.Millisecond}, &diagnostic.RebuildProjectionDetail{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := json.Unmarshal(first, tc.out); err != nil {
				t.Fatalf("unmarshal into %T: %v", tc.out, err)
			}
			second, err := json.Marshal(tc.out)
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			if !bytes.Equal(first, second) {
				t.Fatalf("byte-stable round-trip failed:\n first: %s\nsecond: %s", first, second)
			}
			// reflect-based equality on the dereferenced pointer
			// value catches any field whose JSON tag silently drops
			// data without changing the byte shape.
			roundTripped := reflect.ValueOf(tc.out).Elem().Interface()
			if !reflect.DeepEqual(roundTripped, tc.in) {
				t.Fatalf("round-trip diverged:\n want: %#v\n got:  %#v", tc.in, roundTripped)
			}
		})
	}
}
