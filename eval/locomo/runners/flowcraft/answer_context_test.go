package flowcraft

import (
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	recallv1 "github.com/GizClaw/flowcraft/sdk/recall"
)

var _ runners.AnswerContextRecaller = (*Runner)(nil)

func TestStructuredAnswerBodyKeepsRecallV1HitStructure(t *testing.T) {
	sourceTime := time.Date(2023, 7, 15, 13, 51, 0, 0, time.UTC)
	body := renderStructuredAnswerBody(runners.AnswerQuestion{
		Query:   "When did Melanie go to the pottery workshop?",
		AskedAt: "2023-10-01",
	}, []recallv1.Hit{{
		Entry: recallv1.Entry{
			ID:         "m1",
			Category:   recallv1.CategoryEvents,
			Categories: []string{"event", "activity"},
			Content:    "Last Friday, Melanie took her kids to a pottery workshop.",
			Subject:    "Melanie",
			Predicate:  "went_to",
			Entities:   []string{"Melanie", "pottery workshop"},
			Keywords:   []string{"pottery", "workshop"},
			Confidence: 0.91,
			Source: recallv1.Source{
				Timestamp: sourceTime,
			},
		},
		Score:  0.42,
		Scores: map[string]float64{"bm25": 0.2, "vector": 0.7},
	}})

	for _, want := range []string{
		"ASKED_AT: 2023-10-01",
		"QUESTION: When did Melanie go to the pottery workshop?",
		"MEMORIES (STRUCTURED_ENTRIES):",
		`entry_id: "m1"`,
		`category: "events"`,
		`categories: "activity", "event"`,
		`score: "0.420000"`,
		`scores: bm25=0.200000, vector=0.700000`,
		`content: "Last Friday, Melanie took her kids to a pottery workshop."`,
		`subject: "Melanie"`,
		`predicate: "went_to"`,
		`entities: "Melanie", "pottery workshop"`,
		`keywords: "pottery", "workshop"`,
		`confidence: "0.910"`,
		`source_time: "2023-07-15 13:51"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("structured answer body missing %q:\n%s", want, body)
		}
	}
}

func TestStructuredAnswerContextCarriesRecallV1Prompt(t *testing.T) {
	ctx := structuredAnswerContext(runners.AnswerQuestion{Query: "What happened?"}, nil)
	if ctx.Format != "flowcraftv1_structured_entries" {
		t.Fatalf("unexpected format: %q", ctx.Format)
	}
	for _, want := range []string{
		"structured memory entries",
		"content as the primary evidence",
		"source_time is the timestamp",
		"%s",
	} {
		if !strings.Contains(ctx.PromptTemplate, want) {
			t.Fatalf("structured prompt missing %q:\n%s", want, ctx.PromptTemplate)
		}
	}
}
