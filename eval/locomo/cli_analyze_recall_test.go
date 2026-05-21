package locomo

import "testing"

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
	rec := classifyRecallQuestion(q, "regressed", dump, 2)
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
	rec := classifyRecallQuestion(q, "", dump, 1)
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
