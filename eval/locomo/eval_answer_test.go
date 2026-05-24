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
		directEvidenceSection,
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
		"DIRECT EVIDENCE",
		"SUPPORTING EVIDENCE",
		"LOWER-PRIORITY CONTEXT",
		"Do not answer \"I don't know\"",
		"preserve that relative wording",
		"[source_time:",
	} {
		if !strings.Contains(DefaultAnswerPrompt, want) {
			t.Fatalf("DefaultAnswerPrompt missing %q", want)
		}
	}
}

func TestBuildAnswerBodyOrganizesDirectSupportingAndLowerPriorityContext(t *testing.T) {
	body := buildAnswerBody(dataset.Question{Query: "When did Alice buy ceramic figurines?"}, []runners.Hit{
		{Content: "Alice likes Paris."},
		{Content: "[time: 2023-05-07] Alice bought 2 ceramic figurines."},
		{Content: "Bob cooked soup for Carol."},
		{Content: "Carol visited a museum."},
		{Content: "Dylan likes hiking."},
		{Content: "Eve painted a mural."},
		{Content: "Frank watched a movie."},
		{Content: "Grace read a book."},
		{Content: "[source_time: 2023-05-08 09:00] The receipt was shown later."},
	})

	directIdx := strings.Index(body, directEvidenceSection)
	supportIdx := strings.Index(body, supportingEvidenceSection)
	lowerIdx := strings.Index(body, lowerPrioritySection)
	if directIdx < 0 || supportIdx < 0 || lowerIdx < 0 {
		t.Fatalf("body should contain all organized sections:\n%s", body)
	}
	if !(directIdx < supportIdx && supportIdx < lowerIdx) {
		t.Fatalf("sections should render direct/supporting/lower order:\n%s", body)
	}
	if !strings.Contains(body, "- [#2] [time: 2023-05-07] Alice bought 2 ceramic figurines.") {
		t.Fatalf("strong temporal evidence should keep original rank in direct section:\n%s", body)
	}
	if !strings.Contains(body, "- [#9] [source_time: 2023-05-08 09:00] The receipt was shown later.") {
		t.Fatalf("timestamped context should remain available as supporting evidence:\n%s", body)
	}
	if !strings.Contains(body, "- [#3] Bob cooked soup for Carol.") {
		t.Fatalf("distractor should still be available as lower-priority context:\n%s", body)
	}
}
