package diagnostic_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

// Compile-time assertions: every documented stage Detail satisfies
// StageDetail.
var (
	_ diagnostic.StageDetail = diagnostic.ValidateDetail{}
	_ diagnostic.StageDetail = diagnostic.IngestDetail{}
	_ diagnostic.StageDetail = diagnostic.ResolveDetail{}
	_ diagnostic.StageDetail = diagnostic.AppendDetail{}
	_ diagnostic.StageDetail = diagnostic.ValidityCloseDetail{}
	_ diagnostic.StageDetail = diagnostic.EvidenceMirrorDetail{}
	_ diagnostic.StageDetail = diagnostic.ProjectDetail{}
	_ diagnostic.StageDetail = diagnostic.EvolutionAfterSaveDetail{}
	_ diagnostic.StageDetail = diagnostic.EnqueueSideEffectsDetail{}
	_ diagnostic.StageDetail = diagnostic.ForgetAllDetail{}
	_ diagnostic.StageDetail = diagnostic.ExpireRetiredDetail{}
	_ diagnostic.StageDetail = diagnostic.FeedbackDetail{}
	_ diagnostic.StageDetail = diagnostic.RevisionDetail{}
	_ diagnostic.StageDetail = diagnostic.BuildEpisodeDetail{}
	_ diagnostic.StageDetail = diagnostic.ProjectEpisodeEvidenceDetail{}
	_ diagnostic.StageDetail = diagnostic.EnqueueSemanticDetail{}
	_ diagnostic.StageDetail = diagnostic.OriginStampDetail{}
	_ diagnostic.StageDetail = diagnostic.GraphDependencyDetail{}
	_ diagnostic.StageDetail = diagnostic.AsyncSemanticProcessDetail{}

	_ diagnostic.StageDetail = diagnostic.IntentRouteDetail{}
	_ diagnostic.StageDetail = diagnostic.PlanDetail{}
	_ diagnostic.StageDetail = diagnostic.CandidateFanoutDetail{}
	_ diagnostic.StageDetail = diagnostic.CandidateMergeAndMaterializeDetail{}
	_ diagnostic.StageDetail = diagnostic.PolicyFilterDetail{}
	_ diagnostic.StageDetail = diagnostic.RankDetail{}
	_ diagnostic.StageDetail = diagnostic.ContextPackDetail{}
	_ diagnostic.StageDetail = diagnostic.BuildGroundedHitsDetail{}
	_ diagnostic.StageDetail = diagnostic.EvolutionAfterRecallDetail{}

	_ diagnostic.StageDetail = diagnostic.ScanDetail{}
	_ diagnostic.StageDetail = diagnostic.RebuildProjectionDetail{}
)

// TestStatusDegraded_Value pins the wire-level string the framework emits for
// best-effort failures. Dashboards, telemetry sinks, and external trace
// consumers key off the literal "degraded" value, so a typo here would
// silently break every observer that matches the constant by string rather than
// by Go identifier.
func TestStatusDegraded_Value(t *testing.T) {
	if got := string(diagnostic.StatusDegraded); got != "degraded" {
		t.Fatalf("StatusDegraded = %q, want %q", got, "degraded")
	}
	// Sanity: the new value must not collide with any sibling status.
	for _, other := range []diagnostic.Status{
		diagnostic.StatusOK,
		diagnostic.StatusShortCircuit,
		diagnostic.StatusSkipped,
		diagnostic.StatusFailed,
		diagnostic.StatusCompensated,
	} {
		if other == diagnostic.StatusDegraded {
			t.Errorf("StatusDegraded collides with %q", other)
		}
	}
}

// TestDetail_RoundTrip pins JSON byte-stability for every Detail
// type. Each case constructs a representative non-zero value,
// marshals it, unmarshals into a fresh value of the SAME concrete
// type, then re-marshals and asserts bytes match. Polymorphic
// unmarshal (StageDetail interface) is intentionally NOT tested here; the
// pipeline framework owns that. This test only locks the per-Detail JSON
// contract in place.
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
		{"forget_all", diagnostic.ForgetAllDetail{
			ScopeKey:           "rt-1/u:alice",
			Mode:               "hard",
			Deleted:            42,
			ProjectionsCleared: 6,
			EvidenceCleared:    18,
			Latency:            5 * time.Millisecond,
		}, &diagnostic.ForgetAllDetail{}},
		{"expire_retired", diagnostic.ExpireRetiredDetail{
			ScopeKey:       "rt-1/u:alice",
			ExpiresBefore:  time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC),
			Scanned:        12,
			Deleted:        3,
			ProjectionsHit: 2,
			Latency:        4 * time.Millisecond,
		}, &diagnostic.ExpireRetiredDetail{}},
		{"feedback", diagnostic.FeedbackDetail{
			FactID:             "f-1",
			ReinforcementDelta: 2,
			PenaltyDelta:       0,
			Latency:            1 * time.Millisecond,
		}, &diagnostic.FeedbackDetail{}},
		{"revision", diagnostic.RevisionDetail{
			Kind:          "fork",
			Stage:         "revision_save",
			SourceFactID:  "src-1",
			CreatedFactID: "new-1",
			Latency:       2 * time.Millisecond,
		}, &diagnostic.RevisionDetail{}},
		{"build_episode", diagnostic.BuildEpisodeDetail{
			Turns:          3,
			EpisodeFacts:   1,
			AsyncRequestID: "req-1",
		}, &diagnostic.BuildEpisodeDetail{}},
		{"project_episode_evidence", diagnostic.ProjectEpisodeEvidenceDetail{
			AsyncRequestID: "req-1",
			EpisodeFacts:   1,
			Latency:        2 * time.Millisecond,
		}, &diagnostic.ProjectEpisodeEvidenceDetail{}},
		{"enqueue_semantic", diagnostic.EnqueueSemanticDetail{
			AsyncRequestID: "req-1",
			EpisodeFactIDs: []string{"e1", "e2"},
			Latency:        1 * time.Millisecond,
		}, &diagnostic.EnqueueSemanticDetail{}},
		{"graph_dependencies", diagnostic.GraphDependencyDetail{Checked: 2, MissingDependencies: true, FailedReason: "missing_dependencies", Latency: 2 * time.Millisecond}, &diagnostic.GraphDependencyDetail{}},

		{"intent_route", diagnostic.IntentRouteDetail{
			QueryLen:       22,
			Entities:       []string{"alice"},
			Kinds:          []string{"event", "state"},
			Subject:        "alice",
			HasTimeRange:   false,
			GraphEnabled:   false,
			Strategy:       "default",
			Confidence:     0.7,
			Signals:        []string{"embedding_route"},
			FallbackReason: "",
			Latency:        1 * time.Millisecond,
		}, &diagnostic.IntentRouteDetail{}},
		{"plan", diagnostic.PlanDetail{
			ActivatedLenses: []diagnostic.ActivatedLens{{Lens: "retrieval", Weight: 1.0, Budget: 20, ActivatedBy: "default"}},
			TotalBudget:     20,
		}, &diagnostic.PlanDetail{}},
		{"candidate_fanout", diagnostic.CandidateFanoutDetail{
			SubScopes: []diagnostic.SubScopeRun{{Scope: "rt:u1", SourceResults: 6, Materialized: 5, Latency: 7 * time.Millisecond}},
			Sources:   []diagnostic.SourceResult{{Lens: "retrieval", Candidates: 8, Latency: 3 * time.Millisecond}},
		}, &diagnostic.CandidateFanoutDetail{}},
		{"candidate_merge_and_materialize", diagnostic.CandidateMergeAndMaterializeDetail{InputCount: 30, CandidateCount: 25, MaterializedCount: 9, OutputCount: 5, DroppedByDedup: 2, Latency: 2 * time.Millisecond}, &diagnostic.CandidateMergeAndMaterializeDetail{}},
		{"candidate_expansion", diagnostic.CandidateExpansionDetail{InputCount: 10, OutputCount: 10, Suggested: 2, TaskIntents: []string{"set_completion"}, SuggestedFactIDs: []string{"f1", "f2"}}, &diagnostic.CandidateExpansionDetail{}},
		{"policy_filter", diagnostic.PolicyFilterDetail{MaxSensitivity: "private", ActorID: "agent-a", Removed: 2, Redacted: 1}, &diagnostic.PolicyFilterDetail{}},
		{"rank", diagnostic.RankDetail{InputCount: 9, OutputCount: 9, FinalCap: 10, BoostsApplied: 2, Latency: 1 * time.Millisecond}, &diagnostic.RankDetail{}},
		{"context_pack", diagnostic.ContextPackDetail{Count: 9}, &diagnostic.ContextPackDetail{}},
		{"build_grounded_hits", diagnostic.BuildGroundedHitsDetail{Count: 9}, &diagnostic.BuildGroundedHitsDetail{}},
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
