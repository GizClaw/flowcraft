package diagnostics_test

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/diagnostics"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

func TestAttributeRecallTrace_MapsDrops(t *testing.T) {
	attrs := diagnostics.AttributeRecallTrace(domain.RecallTrace{
		Stages: []diagnostic.StageDiagnostic{{
			Stage: "candidate_merge_and_materialize",
			Detail: diagnostic.CandidateMergeAndMaterializeDetail{
				Drops: []diagnostic.CandidateDrop{{
					Reason: diagnostic.DropStaleFact,
					FactID: "f1",
					Source: "retrieval",
				}},
			},
		}},
	})
	if len(attrs) != 1 || attrs[0].Stage != diagnostics.FailureProjection {
		t.Fatalf("attrs = %+v", attrs)
	}
}

func TestAttributeAnswerContext_FindsDroppedEvidenceGrounding(t *testing.T) {
	hits := []domain.Hit{{
		Fact: domain.TemporalFact{
			ID:           "f1",
			Content:      "Caroline joined a support group",
			EvidenceText: "[9:00 am on 7 May, 2024] Caroline went to the LGBTQ support group.",
			EvidenceRefs: []domain.EvidenceRef{{
				ID:   "D1:3",
				Text: "Caroline said the group met downtown on 7 May.",
			}},
		},
	}}

	attrs := diagnostics.AttributeAnswerContext(hits, []diagnostics.AnswerContextItem{{
		FactID: "f1",
		Text:   "Caroline joined a support group",
	}})
	if len(attrs) != 1 {
		t.Fatalf("attrs = %+v", attrs)
	}
	if attrs[0].Stage != diagnostics.FailureAnswer || attrs[0].Reason != "evidence_grounding_not_rendered" {
		t.Fatalf("unexpected attribution: %+v", attrs[0])
	}
}

func TestAttributeAnswerContext_AllowsGroundedRendering(t *testing.T) {
	hits := []domain.Hit{{
		Fact: domain.TemporalFact{
			ID:           "f1",
			Content:      "Caroline joined a support group",
			EvidenceText: "[9:00 am on 7 May, 2024] Caroline went to the LGBTQ support group.",
		},
	}}

	attrs := diagnostics.AttributeAnswerContext(hits, []diagnostics.AnswerContextItem{{
		FactID: "f1",
		Text:   "Caroline joined a support group [9:00 am on 7 May, 2024] Caroline went to the LGBTQ support group.",
	}})
	if len(attrs) != 0 {
		t.Fatalf("grounded context should not be attributed, got %+v", attrs)
	}
}

func TestDiagnoseRecall_ReportsPerStageHealth(t *testing.T) {
	trace := domain.RecallTrace{
		Stages: []diagnostic.StageDiagnostic{
			{Stage: "plan", Detail: diagnostic.PlanDetail{
				ActivatedLenses: []diagnostic.ActivatedLens{
					{Lens: "retrieval", Budget: 20},
					{Lens: "entity", Budget: 20},
					{Lens: "relation", Budget: 20},
				},
			}},
			{Stage: "candidate_fanout", Detail: diagnostic.CandidateFanoutDetail{
				Sources: []diagnostic.SourceResult{
					{Lens: "retrieval", Candidates: 10},
					{Lens: "entity", Candidates: 0},
				},
			}},
			{Stage: "candidate_merge_and_materialize", Detail: diagnostic.CandidateMergeAndMaterializeDetail{
				CandidateCount:    8,
				MaterializedCount: 6,
				Drops: []diagnostic.CandidateDrop{
					{Stage: "candidate_materialize", Reason: diagnostic.DropStaleFact, FactID: "x"},
					{Stage: "candidate_merge", Reason: diagnostic.DropTotalCap, FactID: "y"},
				},
			}},
		},
	}
	hits := []domain.Hit{
		{Fact: domain.TemporalFact{ID: "f1", Content: "Alice loves Paris"}},
		{Fact: domain.TemporalFact{ID: "f2", Subject: "alice", Predicate: "city", Object: "paris"}},
		{Fact: domain.TemporalFact{ID: "f3"}},
	}

	diag := diagnostics.DiagnoseRecall(trace, hits)
	if len(diag.Sources) != 3 {
		t.Fatalf("sources = %+v", diag.Sources)
	}
	if diag.DropsByStage[diagnostics.FailureProjection] != 1 || diag.DropsByStage[diagnostics.FailureCandidateMerge] != 1 {
		t.Errorf("drops by stage = %+v", diag.DropsByStage)
	}
	if diag.HitRenderability.Total != 3 || diag.HitRenderability.EmptyRenderable != 1 {
		t.Errorf("hit renderability = %+v", diag.HitRenderability)
	}
	if diag.HitRenderability.StructuredOnly != 1 {
		t.Errorf("structured-only count = %+v", diag.HitRenderability)
	}
	if diag.HitRenderability.EmptyTop != 1 {
		t.Errorf("empty-top count = %+v", diag.HitRenderability)
	}
	if diag.CandidateCount != 8 || diag.MaterializedCount != 6 {
		t.Errorf("candidates/materialized = %d/%d", diag.CandidateCount, diag.MaterializedCount)
	}
	if len(diag.Drops) != 2 {
		t.Errorf("drops = %+v", diag.Drops)
	}
}

func TestDiagnoseSave_SummarisesFactQuality(t *testing.T) {
	req := domain.SaveRequest{
		Facts: []domain.TemporalFact{{Kind: domain.KindNote}, {Kind: domain.KindNote}},
		Turns: []domain.TurnContext{{ID: "t1", Text: "hello"}},
	}
	trace := domain.SaveTrace{
		Stages: []diagnostic.StageDiagnostic{
			{Stage: "ingest", Detail: diagnostic.IngestDetail{
				ExtractedFacts: 3,
				Dropped: []diagnostic.DroppedFact{{
					Reason: "policy:reject",
				}},
			}},
			{Stage: "resolve", Detail: diagnostic.ResolveDetail{Appended: 2}},
		},
	}

	diag := diagnostics.DiagnoseSave(req, trace)
	if diag.Input != 3 {
		t.Errorf("input = %d", diag.Input)
	}
	if diag.InputCoverage.Facts != 2 || diag.InputCoverage.Turns != 1 {
		t.Errorf("input coverage facts/turns = %+v", diag.InputCoverage)
	}
	if diag.Compiled.Total != 3 {
		t.Errorf("compiled = %+v", diag.Compiled)
	}
	if diag.Appended.Total != 2 {
		t.Errorf("appended = %+v", diag.Appended)
	}
	if len(diag.Attributions) != 1 || diag.DropsByStage[diag.Attributions[0].Stage] != 1 {
		t.Errorf("drops attribution = %+v %+v", diag.Attributions, diag.DropsByStage)
	}
}

func TestDiagnoseSave_SurfacesStructurizerCoverage(t *testing.T) {
	trace := domain.SaveTrace{
		Stages: []diagnostic.StageDiagnostic{{
			Stage: "ingest",
			Detail: diagnostic.IngestDetail{
				StructurizerCoverage: diagnostic.StructurizerCoverage{
					TotalFactsSeen:      4,
					KindFilled:          2,
					EntitiesFilled:      4,
					SubjectFilled:       3,
					ValidFromHintFilled: 1,
				},
			},
		}},
	}
	diag := diagnostics.DiagnoseSave(domain.SaveRequest{}, trace)
	got := diag.StructurizerCoverage
	if got.TotalFactsSeen != 4 || got.KindFilled != 2 || got.EntitiesFilled != 4 ||
		got.SubjectFilled != 3 || got.ValidFromHintFilled != 1 {
		t.Errorf("structurizer coverage not surfaced verbatim, got %+v", got)
	}
}

func TestPipelineHealth_AggregatesStructurizerCoverage(t *testing.T) {
	h := diagnostics.NewPipelineHealth()
	h.RecordSave(diagnostics.SaveDiagnostics{StructurizerCoverage: diagnostic.StructurizerCoverage{
		TotalFactsSeen: 3, KindFilled: 1, EntitiesFilled: 2, SubjectFilled: 2, ValidFromHintFilled: 0,
	}})
	h.RecordSave(diagnostics.SaveDiagnostics{StructurizerCoverage: diagnostic.StructurizerCoverage{
		TotalFactsSeen: 2, KindFilled: 0, EntitiesFilled: 2, SubjectFilled: 1, ValidFromHintFilled: 1,
	}})
	got := h.StructurizerCoverage
	if got.TotalFactsSeen != 5 || got.KindFilled != 1 || got.EntitiesFilled != 4 ||
		got.SubjectFilled != 3 || got.ValidFromHintFilled != 1 {
		t.Errorf("aggregate structurizer coverage = %+v", got)
	}
}

func TestDiagnoseSave_InputCoverageReflectsTypedChannel(t *testing.T) {
	observed := time.Date(2024, 5, 7, 9, 0, 0, 0, time.UTC)
	req := domain.SaveRequest{
		Turns: []domain.TurnContext{
			{ID: "D1:3", EvidenceID: "D1:3", SessionID: "s1", Role: "user", Speaker: "Alice", Time: observed, Text: "I went to Paris."},
			{ID: "raw-2", Role: "user", Speaker: "Bob", Text: "Me too."},
			{ID: "raw-3", Text: ""},
		},
		ObservedAt: observed,
	}
	trace := domain.SaveTrace{
		Stages: []diagnostic.StageDiagnostic{{
			Stage:  "ingest",
			Detail: diagnostic.IngestDetail{KnownEntitiesSeen: 4},
		}},
	}
	diag := diagnostics.DiagnoseSave(req, trace)

	cov := diag.InputCoverage
	if cov.Turns != 2 {
		t.Errorf("Turns = %d, want 2 (empty Text turn must be skipped)", cov.Turns)
	}
	if cov.TurnsWithTypedTime != 1 || cov.TurnsWithSpeaker != 2 {
		t.Errorf("typed time/speaker = %d/%d, want 1/2", cov.TurnsWithTypedTime, cov.TurnsWithSpeaker)
	}
	if cov.TurnsWithEvidenceID != 1 || cov.TurnsWithSessionID != 1 {
		t.Errorf("evidence/session ids = %d/%d, want 1/1", cov.TurnsWithEvidenceID, cov.TurnsWithSessionID)
	}
	if cov.KnownEntities != 4 {
		t.Errorf("KnownEntities = %d, want 4 (from ingest stage)", cov.KnownEntities)
	}
	if !cov.HasObservedAt {
		t.Errorf("HasObservedAt should reflect non-zero ObservedAt on the request")
	}
}

func TestPipelineHealth_AggregatesSaveAndRecall(t *testing.T) {
	health := diagnostics.NewPipelineHealth()

	health.RecordSave(diagnostics.DiagnoseSave(domain.SaveRequest{Facts: []domain.TemporalFact{{Kind: domain.KindNote}}}, domain.SaveTrace{
		Stages: []diagnostic.StageDiagnostic{
			{Stage: "ingest", Detail: diagnostic.IngestDetail{ExtractedFacts: 2}},
			{Stage: "resolve", Detail: diagnostic.ResolveDetail{Appended: 1}},
		},
	}))
	health.RecordSave(diagnostics.DiagnoseSave(domain.SaveRequest{Turns: []domain.TurnContext{{ID: "t1", Text: "raw"}}}, domain.SaveTrace{
		Stages: []diagnostic.StageDiagnostic{
			{Stage: "ingest", Detail: diagnostic.IngestDetail{ExtractedFacts: 1}},
			{Stage: "resolve", Detail: diagnostic.ResolveDetail{Appended: 1}},
		},
	}))

	health.RecordRecall(diagnostics.DiagnoseRecall(domain.RecallTrace{
		Stages: []diagnostic.StageDiagnostic{
			{Stage: "plan", Detail: diagnostic.PlanDetail{
				ActivatedLenses: []diagnostic.ActivatedLens{{Lens: "retrieval", Budget: 10}},
			}},
			{Stage: "candidate_fanout", Detail: diagnostic.CandidateFanoutDetail{
				Sources: []diagnostic.SourceResult{{Lens: "retrieval", Candidates: 4}},
			}},
		},
	}, []domain.Hit{
		{Fact: domain.TemporalFact{ID: "f1", Content: "alice"}},
		{Fact: domain.TemporalFact{ID: "f2"}},
	}))

	if health.SaveSamples != 2 || health.RecallSamples != 1 {
		t.Fatalf("samples = %d / %d", health.SaveSamples, health.RecallSamples)
	}
	if health.CompiledFacts.Total != 3 || health.AppendedFacts.Total != 2 {
		t.Errorf("totals = %+v %+v", health.CompiledFacts, health.AppendedFacts)
	}
	if health.HitRenderability.Total != 2 || health.HitRenderability.EmptyRenderable != 1 {
		t.Errorf("hit renderability = %+v", health.HitRenderability)
	}
	if health.SourceActivation["retrieval"] != 1 || health.SourceReturned["retrieval"] != 4 {
		t.Errorf("source activation = %+v / %+v", health.SourceActivation, health.SourceReturned)
	}
	if health.InputCoverage.Facts != 1 || health.InputCoverage.Turns != 1 {
		t.Errorf("aggregate input coverage = %+v", health.InputCoverage)
	}
}

func TestDiagnoseRecall_AttributesWinnersToSources(t *testing.T) {
	hits := []domain.Hit{
		{Fact: domain.TemporalFact{ID: "a"}, Sources: []string{"retrieval"}},
		{Fact: domain.TemporalFact{ID: "b"}, Sources: []string{"retrieval", "entity"}},
		{Fact: domain.TemporalFact{ID: "c"}, Sources: []string{"timeline", "retrieval"}},
		{Fact: domain.TemporalFact{ID: "d"}, Sources: []string{"profile"}},
		{Fact: domain.TemporalFact{ID: "e"}},
	}

	diag := diagnostics.DiagnoseRecall(domain.RecallTrace{}, hits)
	if diag.HitProvenance.WinnersBySource["retrieval"] != 3 {
		t.Errorf("retrieval winners = %d", diag.HitProvenance.WinnersBySource["retrieval"])
	}
	if diag.HitProvenance.WinnersBySource["entity"] != 1 {
		t.Errorf("entity winners = %d", diag.HitProvenance.WinnersBySource["entity"])
	}
	if diag.HitProvenance.SoleSourceWinners["retrieval"] != 1 ||
		diag.HitProvenance.SoleSourceWinners["profile"] != 1 {
		t.Errorf("sole-source winners = %+v", diag.HitProvenance.SoleSourceWinners)
	}
	if diag.HitProvenance.MultiSourceWinners != 2 {
		t.Errorf("multi-source winners = %d", diag.HitProvenance.MultiSourceWinners)
	}
	if diag.HitProvenance.NoProvenance != 1 {
		t.Errorf("no-provenance hits = %d", diag.HitProvenance.NoProvenance)
	}
}
