package locomo

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/eval/dataset"
	"github.com/GizClaw/flowcraft/eval/locomo/runners"
)

func TestBuildAnswerBodyAnnotatesMemoryRank(t *testing.T) {
	body := buildAnswerBody(dataset.Question{
		Query:   "When did Alice go hiking?",
		AskedAt: "2023-07-06",
	}, []runners.Hit{{
		Content:     "[time: 2023-07-03] Alice went hiking the week before 6 July 2023.",
		Kind:        "event",
		Sources:     []string{"retrieval", "timeline"},
		EvidenceIDs: []string{"conv-1:D1:3"},
	}})

	for _, want := range []string{
		"ASKED_AT: 2023-07-06",
		"QUESTION: When did Alice go hiking?",
		"ANSWER_HINTS:",
		"likely_when=#1:2023-07-03",
		"relative_candidates=#1:the week before 6 July 2023",
		"[#1] [time: 2023-07-03] Alice went hiking the week before 6 July 2023.",
		"[time: 2023-07-03] Alice went hiking the week before 6 July 2023.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"kind=event", "sources=retrieval", "evidence=conv-1:D1:3"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("body should not include provenance label %q:\n%s", unwanted, body)
		}
	}
}

func TestBuildAnswerBodyNumericHints(t *testing.T) {
	body := buildAnswerBody(dataset.Question{
		Query: "How many times did Alice go to the beach?",
	}, []runners.Hit{{
		Content: "Alice went to the beach 3 times in 2023.",
	}})
	for _, want := range []string{
		"ANSWER_HINTS:",
		"numeric_candidates=#1:3 times",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

func TestBuildAnswerBodyDoesNotPromoteObservationTimeToLikelyWhen(t *testing.T) {
	body := buildAnswerBody(dataset.Question{
		Query: "When did Melanie paint a sunrise?",
	}, []runners.Hit{{
		Content: "[observed_at: 2023-05-08] | evidence: Melanie painted a lake sunrise last year. | evidence: [source_time: 2023-05-08 13:56] I painted that lake sunrise last year!",
	}})

	for _, want := range []string{
		"relative_candidates=#1:last year",
		"weak_observed_at=#1:2023-05-08",
		"source_time_anchors=#1:2023-05-08",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "likely_when=#1:2023-05-08") {
		t.Fatalf("observed/source time must not be promoted to likely_when:\n%s", body)
	}
}

func TestBuildAnswerBodyLimitsWhenHintsToTopRankedMemories(t *testing.T) {
	hits := []runners.Hit{
		{Content: "[observed_at: 2023-05-01] unrelated memory 1."},
		{Content: "[observed_at: 2023-05-02] unrelated memory 2."},
		{Content: "[observed_at: 2023-05-03] unrelated memory 3."},
		{Content: "[observed_at: 2023-05-04] unrelated memory 4."},
		{Content: "[observed_at: 2023-05-05] unrelated memory 5."},
		{Content: "[observed_at: 2023-05-06] unrelated memory 6."},
		{Content: "[observed_at: 2023-05-07] unrelated memory 7."},
		{Content: "Melanie painted a sunrise last year."},
		{Content: "[time: 2023-05-09] Low-ranked distractor date."},
	}
	body := buildAnswerBody(dataset.Question{
		Query: "When did Melanie paint a sunrise?",
	}, hits)

	if !strings.Contains(body, "relative_candidates=#8:last year") {
		t.Fatalf("body should keep top-ranked relative hint:\n%s", body)
	}
	for _, unwanted := range []string{
		"likely_when=#9:2023-05-09",
		"weak_observed_at=#9:",
	} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("body should trim low-ranked when hint %q:\n%s", unwanted, body)
		}
	}
}

func TestDefaultAnswerPromptMentionsRankedEvidenceAndRelativeDates(t *testing.T) {
	for _, want := range []string{
		"Prefer lower-numbered memories",
		"combine the supported items",
		"Do not answer \"I don't know\"",
		"preserve that relative wording",
		"[observed_at:",
		"weak observation stamp",
		"[source_time:",
		"never answer a WHEN question from source_time alone",
	} {
		if !strings.Contains(DefaultAnswerPrompt, want) {
			t.Fatalf("DefaultAnswerPrompt missing %q", want)
		}
	}
}
