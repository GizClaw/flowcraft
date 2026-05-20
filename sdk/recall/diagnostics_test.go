package recall

import (
	"context"
	"testing"
	"time"
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
	req := SaveRequest{Facts: []TemporalFact{{Kind: FactNote}, {Kind: FactNote}}, Turns: []TurnContext{{ID: "t1", Text: "hello"}}}
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
	if diag.InputCoverage.Facts != 2 || diag.InputCoverage.Turns != 1 {
		t.Errorf("input coverage facts/turns = %+v", diag.InputCoverage)
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

func TestDiagnoseSave_SurfacesStructurizerCoverage(t *testing.T) {
	// Structurizer coverage is the only attributable signal between
	// the 4 sub-tasks (Kind / Entities / Subject / ValidFrom). The
	// diagnostics layer must pass the compiler-side counters through
	// verbatim so a single SaveTrace tells operators which stage of
	// the Structurizer actually did work on this Save.
	trace := SaveTrace{
		StructurizerCoverage: StructurizerCoverage{
			TotalFactsSeen:      4,
			KindFilled:          2,
			EntitiesFilled:      4,
			SubjectFilled:       3,
			ValidFromHintFilled: 1,
		},
	}
	diag := DiagnoseSave(SaveRequest{}, trace)
	got := diag.StructurizerCoverage
	if got.TotalFactsSeen != 4 || got.KindFilled != 2 || got.EntitiesFilled != 4 ||
		got.SubjectFilled != 3 || got.ValidFromHintFilled != 1 {
		t.Errorf("structurizer coverage not surfaced verbatim, got %+v", got)
	}
}

func TestPipelineHealth_AggregatesStructurizerCoverage(t *testing.T) {
	// PipelineHealth must sum per-Save Structurizer coverage across
	// the whole workload so the JSON report exposes fill-rate
	// ratios over many Saves, not just one.
	h := NewPipelineHealth()
	h.RecordSave(SaveDiagnostics{StructurizerCoverage: StructurizerCoverage{
		TotalFactsSeen: 3, KindFilled: 1, EntitiesFilled: 2, SubjectFilled: 2, ValidFromHintFilled: 0,
	}})
	h.RecordSave(SaveDiagnostics{StructurizerCoverage: StructurizerCoverage{
		TotalFactsSeen: 2, KindFilled: 0, EntitiesFilled: 2, SubjectFilled: 1, ValidFromHintFilled: 1,
	}})
	got := h.StructurizerCoverage
	if got.TotalFactsSeen != 5 || got.KindFilled != 1 || got.EntitiesFilled != 4 ||
		got.SubjectFilled != 3 || got.ValidFromHintFilled != 1 {
		t.Errorf("aggregate structurizer coverage = %+v", got)
	}
}

func TestDiagnoseSave_InputCoverageReflectsTypedChannel(t *testing.T) {
	// The typed channel is the whole architecture: when an adapter
	// forgets to populate Time / Speaker / EvidenceID, the LLM
	// falls back to grep-prose mode and quality silently drops.
	// InputCoverage must surface the per-field coverage so that
	// regression is attributable.
	observed := time.Date(2024, 5, 7, 9, 0, 0, 0, time.UTC)
	req := SaveRequest{
		Turns: []TurnContext{
			// Fully-typed: Time + Speaker + EvidenceID + SessionID.
			{ID: "D1:3", EvidenceID: "D1:3", SessionID: "s1", Role: "user", Speaker: "Alice", Time: observed, Text: "I went to Paris."},
			// Speaker-only: no Time / IDs (degraded adapter).
			{ID: "raw-2", Role: "user", Speaker: "Bob", Text: "Me too."},
			// Empty Text — must NOT be counted.
			{ID: "raw-3", Text: ""},
		},
		ObservedAt: observed,
	}
	trace := SaveTrace{KnownEntitiesSeen: 4}
	diag := DiagnoseSave(req, trace)

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
		t.Errorf("KnownEntities = %d, want 4 (from SaveTrace)", cov.KnownEntities)
	}
	if !cov.HasObservedAt {
		t.Errorf("HasObservedAt should reflect non-zero ObservedAt on the request")
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
	health.RecordSave(DiagnoseSave(SaveRequest{Turns: []TurnContext{{ID: "t1", Text: "raw"}}}, SaveTrace{
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
	// Two Save calls fed 1 fact + 1 turn = aggregate Facts=1, Turns=1.
	if health.InputCoverage.Facts != 1 || health.InputCoverage.Turns != 1 {
		t.Errorf("aggregate input coverage = %+v", health.InputCoverage)
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
