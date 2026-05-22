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

func TestDefaultAnswerPromptMentionsRankedEvidenceAndRelativeDates(t *testing.T) {
	for _, want := range []string{
		"Prefer lower-numbered memories",
		"combine the supported items",
		"Do not answer \"I don't know\"",
		"READING_HINTS_FROM_MEMORIES",
		"preserve that relative wording",
		"[source_time:",
	} {
		if !strings.Contains(DefaultAnswerPrompt, want) {
			t.Fatalf("DefaultAnswerPrompt missing %q", want)
		}
	}
}

func TestBuildAnswerBodyEvidenceFirstClustersSourceTurn(t *testing.T) {
	body := buildAnswerBody(dataset.Question{Query: "Where did Alice go?"}, []runners.Hit{
		{
			Content:     "[time: 2024-05-07] Alice went to Tampa. | evidence: [source_time: 2024-05-01 09:30] I am visiting Tampa next week.",
			EvidenceIDs: []string{"D1:7"},
		},
		{
			Content:     "Alice planned travel with Bob. | evidence: [source_time: 2024-05-01 09:30] I am visiting Tampa next week.",
			EvidenceIDs: []string{"D1:7"},
		},
	})
	if strings.Contains(body, "[#2]") {
		t.Fatalf("same source turn should be clustered into one memory group:\n%s", body)
	}
	evidenceIdx := strings.Index(body, "[source_time: 2024-05-01 09:30]")
	memoryIdx := strings.Index(body, "memory: Alice went to Tampa.")
	if evidenceIdx < 0 || memoryIdx < 0 || evidenceIdx > memoryIdx {
		t.Fatalf("evidence should render before abstract memory summary:\n%s", body)
	}
	if !strings.Contains(body, "Alice planned travel with Bob.") {
		t.Fatalf("clustered summary should keep the second fact:\n%s", body)
	}
}

func TestBuildAnswerBodyAddsTemporalAndNumericHints(t *testing.T) {
	body := buildAnswerBody(dataset.Question{
		Query:   "How many tickets did Alice buy?",
		AskedAt: "2024-05-03",
	}, []runners.Hit{{
		Content:     "[time: 2024-05-02] Alice bought 2 tickets. | evidence: [source_time: 2024-05-02 10:00] I bought 2 tickets yesterday.",
		ValidFrom:   "2024-05-02",
		EvidenceIDs: []string{"D1:2"},
	}})
	for _, want := range []string{
		"READING_HINTS_FROM_MEMORIES:",
		"- temporal: [#1] time=2024-05-02, source_time=2024-05-02 10:00",
		"- numeric: [#1] 2",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}
