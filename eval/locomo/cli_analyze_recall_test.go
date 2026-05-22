package locomo

import (
	"testing"

	"github.com/GizClaw/flowcraft/eval/dataset"
)

func TestClassifyRecallQuestion_MissTypesAndTermRanks(t *testing.T) {
	q := QuestionScore{
		ID:         "q1",
		Query:      "What did Alice read?",
		Prediction: "I don't know.",
		Judge:      0,
	}
	dump := recallDumpRecord{
		QID:  "q1",
		Gold: []string{"Charlotte's Web"},
		Hits: []recallDumpHit{
			{ID: "h1", Rank: 1, Kind: "state", Sources: []string{"retrieval"}, Content: "Alice likes books."},
			{ID: "h2", Rank: 2, Kind: "event", Sources: []string{"graph"}, Content: "Alice read Charlotte's Web as a child."},
		},
	}
	rec := classifyRecallQuestion(q, "regressed", dump, 2, auditSignals{}, nil, nil, stageAuditDumpRecord{})
	if rec.MissType != "answer_abstain_gold_terms_present" {
		t.Fatalf("miss_type = %q", rec.MissType)
	}
	if rec.BestGoldRank != 2 {
		t.Fatalf("best_gold_rank = %d, want 2", rec.BestGoldRank)
	}
	if rec.TermCoverage != 1 {
		t.Fatalf("gold_term_coverage = %f, want 1", rec.TermCoverage)
	}
	if len(rec.MissingTerms) != 0 {
		t.Fatalf("missing terms = %+v, want none", rec.MissingTerms)
	}
	if len(rec.TermHits) == 0 || rec.TermHits[0].Term != "charlotte's" || rec.TermHits[0].Rank != 2 {
		t.Fatalf("term hits = %+v", rec.TermHits)
	}
	if rec.TopKinds["event"] != 1 || rec.TopSources["graph"] != 1 {
		t.Fatalf("kind/source counts = %+v / %+v", rec.TopKinds, rec.TopSources)
	}
}

func TestClassifyRecallQuestion_AbsentGoldTermsIsRecallMiss(t *testing.T) {
	q := QuestionScore{
		ID:         "q1",
		Query:      "Where did Alice go?",
		Prediction: "Paris.",
		Judge:      0,
	}
	dump := recallDumpRecord{
		QID:  "q1",
		Gold: []string{"Tampa, Florida"},
		Hits: []recallDumpHit{{
			ID:      "h1",
			Rank:    1,
			Kind:    "event",
			Sources: []string{"retrieval"},
			Content: "Alice went to Paris.",
		}},
	}
	rec := classifyRecallQuestion(q, "", dump, 1, auditSignals{}, nil, nil, stageAuditDumpRecord{})
	if rec.MissType != "recall_miss_gold_terms_absent" {
		t.Fatalf("miss_type = %q", rec.MissType)
	}
	if rec.BestGoldRank != 0 {
		t.Fatalf("best_gold_rank = %d, want 0", rec.BestGoldRank)
	}
	if rec.TermCoverage != 0 || len(rec.MissingTerms) != 2 {
		t.Fatalf("coverage/missing = %f / %+v, want 0 and two terms", rec.TermCoverage, rec.MissingTerms)
	}
}

func TestClassifyRecallQuestion_ExtractMissBeatsRecallMiss(t *testing.T) {
	q := QuestionScore{
		ID:         "q1",
		Query:      "Where did Alice go?",
		Prediction: "I don't know.",
		Judge:      0,
	}
	dump := recallDumpRecord{
		QID:  "q1",
		Gold: []string{"Tampa"},
		Hits: []recallDumpHit{{
			ID:      "h1",
			Rank:    1,
			Content: "Alice went to Paris.",
		}},
	}
	signals := auditSignals{
		ConversationID: "conv-1",
		EvidenceIDs:    []string{"e1"},
		GoldTerms:      []string{"tampa"},
		EvidenceTerms:  []string{"flight", "tampa"},
	}
	facts := map[string][]factDumpFact{
		"conv-1": {{
			ID:      "f1",
			Content: "Alice booked a hotel in Paris.",
		}},
	}
	rec := classifyRecallQuestion(q, "", dump, 1, signals, facts, nil, stageAuditDumpRecord{})
	if rec.MissType != "extract_miss" {
		t.Fatalf("miss_type = %q", rec.MissType)
	}
	if rec.ExtractStatus != "extract_miss" {
		t.Fatalf("extract_status = %q", rec.ExtractStatus)
	}
}

func TestClassifyRecallQuestion_FactsPresentButNotRecalled(t *testing.T) {
	q := QuestionScore{
		ID:         "q1",
		Query:      "Where did Alice go?",
		Prediction: "I don't know.",
		Judge:      0,
	}
	dump := recallDumpRecord{
		QID:  "q1",
		Gold: []string{"Tampa"},
		Hits: []recallDumpHit{{
			ID:      "h1",
			Rank:    1,
			Content: "Alice went to Paris.",
		}},
	}
	signals := auditSignals{
		ConversationID: "conv-1",
		EvidenceIDs:    []string{"e1"},
		GoldTerms:      []string{"tampa"},
		EvidenceTerms:  []string{"flight", "tampa"},
	}
	facts := map[string][]factDumpFact{
		"conv-1": {{
			ID:          "f1",
			Content:     "Alice booked a flight to Tampa.",
			EvidenceIDs: []string{"e1"},
		}},
	}
	rec := classifyRecallQuestion(q, "", dump, 1, signals, facts, nil, stageAuditDumpRecord{})
	if rec.MissType != "recall_miss" {
		t.Fatalf("miss_type = %q", rec.MissType)
	}
	if rec.ExtractStatus != "extract_hit_evidence_id" {
		t.Fatalf("extract_status = %q", rec.ExtractStatus)
	}
	if rec.ExtractTermCoverage != 1 {
		t.Fatalf("extract_term_coverage = %f, want 1", rec.ExtractTermCoverage)
	}
}

func TestBuildAuditSignals_MapsEvidenceTerms(t *testing.T) {
	ds := &dataset.Dataset{
		Conversations: []dataset.Conversation{{
			ID: "conv-1",
			Turns: []dataset.Turn{{
				EvidenceID: "e1",
				Content:    "Alice booked a flight to Tampa in June.",
			}},
		}},
		Questions: []dataset.Question{{
			ID:             "q1",
			ConversationID: "conv-1",
			GoldAnswers:    []string{"Tampa"},
			EvidenceIDs:    []string{"e1"},
		}},
	}
	signals := buildAuditSignals(ds)["q1"]
	if signals.ConversationID != "conv-1" {
		t.Fatalf("conversation_id = %q", signals.ConversationID)
	}
	if len(signals.EvidenceIDs) != 1 || signals.EvidenceIDs[0] != "e1" {
		t.Fatalf("evidence_ids = %+v", signals.EvidenceIDs)
	}
	if !containsString(signals.EvidenceTerms, "tampa") || !containsString(signals.EvidenceTerms, "flight") {
		t.Fatalf("evidence_terms = %+v", signals.EvidenceTerms)
	}
}

func TestClassifyRecallQuestion_StageAuditFindsFusionDrop(t *testing.T) {
	q := QuestionScore{
		ID:         "q1",
		Query:      "Where did Alice go?",
		Prediction: "I don't know.",
		Judge:      0,
	}
	dump := recallDumpRecord{QID: "q1", Gold: []string{"Tampa"}, Hits: []recallDumpHit{{ID: "f2", Content: "Alice went to Paris."}}}
	signals := auditSignals{
		ConversationID: "conv-1",
		EvidenceIDs:    []string{"e1"},
		GoldTerms:      []string{"tampa"},
	}
	facts := map[string][]factDumpFact{"conv-1": {{ID: "f1", Content: "Alice went to Tampa.", EvidenceIDs: []string{"e1"}}}}
	factByID := map[string]factDumpFact{"f1": facts["conv-1"][0]}
	stageAudit := stageAuditDumpRecord{QID: "q1", Stages: []stageAuditDumpStage{
		{Stage: "source_fanout", Source: "retrieval", Candidates: []stageAuditDumpCandidate{{FactID: "f1", EvidenceIDs: []string{"e1"}}}},
		{Stage: "fusion", Candidates: []stageAuditDumpCandidate{{FactID: "f2"}}},
		{Stage: "build_hits", Candidates: []stageAuditDumpCandidate{{FactID: "f2"}}},
	}}
	rec := classifyRecallQuestion(q, "", dump, 1, signals, facts, factByID, stageAudit)
	if rec.MissType != "fusion_drop_evidence_id" {
		t.Fatalf("miss_type = %q", rec.MissType)
	}
	if rec.StageMiss != "fusion_drop_evidence_id" {
		t.Fatalf("stage_miss = %q", rec.StageMiss)
	}
}

func TestClassifyRecallQuestion_StageAuditFindsSourceMiss(t *testing.T) {
	q := QuestionScore{ID: "q1", Query: "Where did Alice go?", Prediction: "Paris.", Judge: 0}
	dump := recallDumpRecord{QID: "q1", Gold: []string{"Tampa"}, Hits: []recallDumpHit{{ID: "f2", Content: "Alice went to Paris."}}}
	signals := auditSignals{
		ConversationID: "conv-1",
		EvidenceIDs:    []string{"e1"},
		GoldTerms:      []string{"tampa"},
	}
	facts := map[string][]factDumpFact{"conv-1": {{ID: "f1", Content: "Alice went to Tampa.", EvidenceIDs: []string{"e1"}}}}
	factByID := map[string]factDumpFact{"f1": facts["conv-1"][0]}
	stageAudit := stageAuditDumpRecord{QID: "q1", Stages: []stageAuditDumpStage{
		{Stage: "source_fanout", Source: "retrieval", Candidates: []stageAuditDumpCandidate{{FactID: "f2"}}},
		{Stage: "fusion", Candidates: []stageAuditDumpCandidate{{FactID: "f2"}}},
		{Stage: "build_hits", Candidates: []stageAuditDumpCandidate{{FactID: "f2"}}},
	}}
	rec := classifyRecallQuestion(q, "", dump, 1, signals, facts, factByID, stageAudit)
	if rec.MissType != "source_miss_evidence_id" {
		t.Fatalf("miss_type = %q", rec.MissType)
	}
}

func TestClassifyRecallQuestion_StageAuditEvidenceInFinalIsAnswerMiss(t *testing.T) {
	q := QuestionScore{ID: "q1", Query: "Where did Alice go?", Prediction: "Paris.", Judge: 0}
	dump := recallDumpRecord{QID: "q1", Gold: []string{"Tampa"}, Hits: []recallDumpHit{{ID: "f1", Content: "Alice went to Tampa."}}}
	signals := auditSignals{
		ConversationID: "conv-1",
		EvidenceIDs:    []string{"e1"},
		GoldTerms:      []string{"tampa"},
	}
	facts := map[string][]factDumpFact{"conv-1": {{ID: "f1", Content: "Alice went to Tampa.", EvidenceIDs: []string{"e1"}}}}
	factByID := map[string]factDumpFact{"f1": facts["conv-1"][0]}
	stageAudit := stageAuditDumpRecord{QID: "q1", Stages: []stageAuditDumpStage{
		{Stage: "source_fanout", Source: "retrieval", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "fusion", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "materialize", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "rank_output", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "build_hits", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
	}}
	rec := classifyRecallQuestion(q, "", dump, 1, signals, facts, factByID, stageAudit)
	if rec.MissType != "answer_miss" {
		t.Fatalf("miss_type = %q", rec.MissType)
	}
	if rec.StageMiss != "" {
		t.Fatalf("stage_miss = %q", rec.StageMiss)
	}
}

func TestClassifyRecallQuestion_StageAuditFindsRerankDrop(t *testing.T) {
	q := QuestionScore{ID: "q1", Query: "Where did Alice go?", Prediction: "Paris.", Judge: 0}
	dump := recallDumpRecord{QID: "q1", Gold: []string{"Tampa"}, Hits: []recallDumpHit{{ID: "f2", Content: "Alice went to Paris."}}}
	signals := auditSignals{ConversationID: "conv-1", EvidenceIDs: []string{"e1"}, GoldTerms: []string{"tampa"}}
	facts := map[string][]factDumpFact{"conv-1": {{ID: "f1", Content: "Alice went to Tampa.", EvidenceIDs: []string{"e1"}}}}
	factByID := map[string]factDumpFact{"f1": facts["conv-1"][0]}
	stageAudit := stageAuditDumpRecord{QID: "q1", Stages: []stageAuditDumpStage{
		{Stage: "source_fanout", Source: "retrieval", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "fusion", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "materialize", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "rank_output", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "build_hits_input", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "build_hits_reranked", Candidates: []stageAuditDumpCandidate{{FactID: "f2"}}},
		{Stage: "build_hits", Candidates: []stageAuditDumpCandidate{{FactID: "f2"}}},
	}}
	rec := classifyRecallQuestion(q, "", dump, 1, signals, facts, factByID, stageAudit)
	if rec.MissType != "rerank_drop_evidence_id" {
		t.Fatalf("miss_type = %q", rec.MissType)
	}
}

func TestClassifyRecallQuestion_StageAuditFindsFinalLimitDrop(t *testing.T) {
	q := QuestionScore{ID: "q1", Query: "Where did Alice go?", Prediction: "Paris.", Judge: 0}
	dump := recallDumpRecord{QID: "q1", Gold: []string{"Tampa"}, Hits: []recallDumpHit{{ID: "f2", Content: "Alice went to Paris."}}}
	signals := auditSignals{ConversationID: "conv-1", EvidenceIDs: []string{"e1"}, GoldTerms: []string{"tampa"}}
	facts := map[string][]factDumpFact{"conv-1": {{ID: "f1", Content: "Alice went to Tampa.", EvidenceIDs: []string{"e1"}}}}
	factByID := map[string]factDumpFact{"f1": facts["conv-1"][0]}
	stageAudit := stageAuditDumpRecord{QID: "q1", Stages: []stageAuditDumpStage{
		{Stage: "source_fanout", Source: "retrieval", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "fusion", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "materialize", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "rank_output", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "build_hits_input", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "build_hits_reranked", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "build_hits", Candidates: []stageAuditDumpCandidate{{FactID: "f2"}}},
	}}
	rec := classifyRecallQuestion(q, "", dump, 1, signals, facts, factByID, stageAudit)
	if rec.MissType != "final_limit_drop_evidence_id" {
		t.Fatalf("miss_type = %q", rec.MissType)
	}
}

func TestClassifyRecallQuestion_StageAuditFindsBuildHitsInconsistency(t *testing.T) {
	q := QuestionScore{ID: "q1", Query: "Where did Alice go?", Prediction: "Paris.", Judge: 0}
	dump := recallDumpRecord{QID: "q1", Gold: []string{"Tampa"}, Hits: []recallDumpHit{{ID: "f2", Content: "Alice went to Paris."}}}
	signals := auditSignals{ConversationID: "conv-1", EvidenceIDs: []string{"e1"}, GoldTerms: []string{"tampa"}}
	facts := map[string][]factDumpFact{"conv-1": {{ID: "f1", Content: "Alice went to Tampa.", EvidenceIDs: []string{"e1"}}}}
	factByID := map[string]factDumpFact{"f1": facts["conv-1"][0]}
	stageAudit := stageAuditDumpRecord{QID: "q1", Stages: []stageAuditDumpStage{
		{Stage: "source_fanout", Source: "retrieval", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "fusion", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "materialize", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "rank_output", Candidates: []stageAuditDumpCandidate{{FactID: "f1"}}},
		{Stage: "build_hits_input", Candidates: []stageAuditDumpCandidate{{FactID: "f2"}}},
		{Stage: "build_hits", Candidates: []stageAuditDumpCandidate{{FactID: "f2"}}},
	}}
	rec := classifyRecallQuestion(q, "", dump, 1, signals, facts, factByID, stageAudit)
	if rec.MissType != "audit_inconsistent_evidence_id" {
		t.Fatalf("miss_type = %q", rec.MissType)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
