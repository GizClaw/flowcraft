package recall

import (
	"context"
	"testing"
)

func TestAttributeRecallTrace_PublicWrapper(t *testing.T) {
	attrs := AttributeRecallTrace(RecallTrace{
		Drops: []CandidateDrop{{
			Reason: DropStaleFact,
			FactID: "f1",
			Source: "retrieval",
		}},
	})
	if len(attrs) != 1 || attrs[0].Stage != FailureProjection {
		t.Fatalf("attrs = %+v", attrs)
	}
}

func TestAttributeAnswerContext_FindsDroppedEvidenceGrounding(t *testing.T) {
	hits := []Hit{{
		Fact: TemporalFact{
			ID:           "f1",
			Content:      "Caroline joined a support group",
			EvidenceText: "[9:00 am on 7 May, 2024] Caroline went to the LGBTQ support group.",
			EvidenceRefs: []EvidenceRef{{
				ID:   "D1:3",
				Text: "Caroline said the group met downtown on 7 May.",
			}},
		},
	}}

	attrs := AttributeAnswerContext(hits, []AnswerContextItem{{
		FactID: "f1",
		Text:   "Caroline joined a support group",
	}})
	if len(attrs) != 1 {
		t.Fatalf("attrs = %+v", attrs)
	}
	if attrs[0].Stage != FailureAnswer || attrs[0].Reason != "evidence_grounding_not_rendered" {
		t.Fatalf("unexpected attribution: %+v", attrs[0])
	}
}

func TestAttributeAnswerContext_AllowsGroundedRendering(t *testing.T) {
	hits := []Hit{{
		Fact: TemporalFact{
			ID:           "f1",
			Content:      "Caroline joined a support group",
			EvidenceText: "[9:00 am on 7 May, 2024] Caroline went to the LGBTQ support group.",
		},
	}}

	attrs := AttributeAnswerContext(hits, []AnswerContextItem{{
		FactID: "f1",
		Text:   "Caroline joined a support group [9:00 am on 7 May, 2024] Caroline went to the LGBTQ support group.",
	}})
	if len(attrs) != 0 {
		t.Fatalf("grounded context should not be attributed, got %+v", attrs)
	}
}

func TestDiagnoseRecall_ReportsPerStageHealth(t *testing.T) {
	trace := RecallTrace{
		Plan: QueryPlan{
			SourceOrder: []string{"retrieval", "entity", "relation"},
			SourceBudgets: map[string]int{
				"retrieval": 20,
				"entity":    20,
				"relation":  20,
			},
		},
		Sources: []SourceTrace{
			{Source: "retrieval", Budget: 20, Returned: 10},
			{Source: "entity", Budget: 20, Returned: 0},
		},
		FusedCandidates: 8,
		Materialized:    6,
		Drops: []CandidateDrop{
			{Stage: "materialize", Reason: DropStaleFact, FactID: "x"},
			{Stage: "fusion", Reason: DropTotalCap, FactID: "y"},
		},
	}
	hits := []Hit{
		{Fact: TemporalFact{ID: "f1", Content: "Alice loves Paris"}},
		{Fact: TemporalFact{ID: "f2", Subject: "alice", Predicate: "city", Object: "paris"}},
		{Fact: TemporalFact{ID: "f3"}},
	}

	diag := DiagnoseRecall(trace, hits)
	if len(diag.Sources) != 3 {
		t.Fatalf("sources = %+v", diag.Sources)
	}
	var retrieval, relation SourceDiagnostic
	for _, s := range diag.Sources {
		switch s.Source {
		case "retrieval":
			retrieval = s
		case "relation":
			relation = s
		}
	}
	if !retrieval.Activated || retrieval.Returned != 10 {
		t.Errorf("retrieval diag = %+v", retrieval)
	}
	if relation.Activated {
		t.Errorf("relation should not be activated, got %+v", relation)
	}
	if diag.DropsByStage[FailureProjection] != 1 || diag.DropsByStage[FailureFusion] != 1 {
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
}

func TestDiagnoseSave_SummarisesFactQuality(t *testing.T) {
	req := SaveRequest{Facts: []TemporalFact{{Kind: FactNote}, {Kind: FactNote}}, Text: "hello"}
	trace := SaveTrace{
		CompiledFacts: []TemporalFact{
			{ID: "f1", Kind: FactEvent, Content: "Caroline joined a support group", EvidenceText: "src", Confidence: 0.8},
			{ID: "f2", Kind: FactState, Subject: "alice", Predicate: "city", Object: "paris"},
			{ID: "f3", Kind: FactNote},
		},
		Appended: []TemporalFact{
			{ID: "f1", Kind: FactEvent, Content: "Caroline joined a support group", EvidenceText: "src", Confidence: 0.8},
			{ID: "f2", Kind: FactState, Subject: "alice", Predicate: "city", Object: "paris"},
		},
		Dropped: []DroppedFact{{Fact: TemporalFact{Kind: FactNote}, Reason: "policy_reject"}},
	}

	diag := DiagnoseSave(req, trace)
	if diag.Input != 3 {
		t.Errorf("input = %d", diag.Input)
	}
	if diag.Compiled.Total != 3 || diag.Compiled.WithContent != 1 ||
		diag.Compiled.StructuredOnly != 1 || diag.Compiled.EmptyRenderable != 1 {
		t.Errorf("compiled = %+v", diag.Compiled)
	}
	if diag.Compiled.WithEvidence != 1 || diag.Compiled.WithConfidence != 1 {
		t.Errorf("compiled signals = %+v", diag.Compiled)
	}
	if diag.Compiled.ByKind[string(FactEvent)] != 1 || diag.Compiled.ByKind[string(FactState)] != 1 {
		t.Errorf("by kind = %+v", diag.Compiled.ByKind)
	}
	if diag.Appended.Total != 2 || diag.Appended.EmptyRenderable != 0 {
		t.Errorf("appended = %+v", diag.Appended)
	}
	if len(diag.Attributions) != 1 || diag.DropsByStage[diag.Attributions[0].Stage] != 1 {
		t.Errorf("drops attribution = %+v %+v", diag.Attributions, diag.DropsByStage)
	}
}

func TestPipelineHealth_AggregatesSaveAndRecall(t *testing.T) {
	health := NewPipelineHealth()

	health.RecordSave(DiagnoseSave(SaveRequest{Facts: []TemporalFact{{Kind: FactNote}}}, SaveTrace{
		CompiledFacts: []TemporalFact{
			{Kind: FactEvent, Content: "alice met bob"},
			{Kind: FactState, Subject: "alice", Predicate: "city", Object: "paris"},
		},
		Appended: []TemporalFact{
			{Kind: FactEvent, Content: "alice met bob"},
		},
	}))
	health.RecordSave(DiagnoseSave(SaveRequest{Text: "raw"}, SaveTrace{
		CompiledFacts: []TemporalFact{{Kind: FactNote}},
		Appended:      []TemporalFact{{Kind: FactNote}},
	}))

	health.RecordRecall(DiagnoseRecall(RecallTrace{
		Plan: QueryPlan{SourceOrder: []string{"retrieval"}, SourceBudgets: map[string]int{"retrieval": 10}},
		Sources: []SourceTrace{
			{Source: "retrieval", Budget: 10, Returned: 4},
		},
	}, []Hit{
		{Fact: TemporalFact{ID: "f1", Content: "alice"}},
		{Fact: TemporalFact{ID: "f2"}},
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
}

func TestDiagnoseRecall_AttributesWinnersToSources(t *testing.T) {
	hits := []Hit{
		{Fact: TemporalFact{ID: "a"}, Sources: []string{"retrieval"}},
		{Fact: TemporalFact{ID: "b"}, Sources: []string{"retrieval", "entity"}},
		{Fact: TemporalFact{ID: "c"}, Sources: []string{"timeline", "retrieval"}},
		{Fact: TemporalFact{ID: "d"}, Sources: []string{"profile"}},
		{Fact: TemporalFact{ID: "e"}}, // no provenance
	}

	diag := DiagnoseRecall(RecallTrace{}, hits)
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

func TestRepairPlanFromTrace(t *testing.T) {
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	plan := RepairPlanFromTrace(scope, RecallTrace{
		Drops: []CandidateDrop{{Reason: DropStaleFact, FactID: "stale"}},
	})
	if len(plan.FactIDs) != 1 || plan.FactIDs[0] != "stale" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestWithGovernance_RejectsOnSave(t *testing.T) {
	g := DefaultGovernance()
	g.Write = rejectAllWritePolicy{}
	mem, err := New(WithGovernance(g))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()

	res, err := mem.Save(context.Background(), Scope{RuntimeID: "rt", UserID: "u1"}, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "secret"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.FactIDs) != 0 {
		t.Fatalf("governance reject should yield empty save, got %+v", res)
	}
}

type rejectAllWritePolicy struct{}

func (rejectAllWritePolicy) Apply(TemporalFact) (TemporalFact, bool) {
	return TemporalFact{}, false
}
