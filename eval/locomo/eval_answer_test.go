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
		t.Fatalf("timestamp-only context should remain available in lower-priority context:\n%s", body)
	}
	if !strings.Contains(body, "- [#3] Bob cooked soup for Carol.") {
		t.Fatalf("distractor should still be available as lower-priority context:\n%s", body)
	}
}

func TestBuildAnswerBodyCapsDirectEvidence(t *testing.T) {
	hits := []runners.Hit{
		{Content: "[time: 2023-05-08] Caroline is going to do some research."},
		{Content: "[time: 2023-05-25] Caroline researched adoption agencies."},
	}
	for _, content := range []string{
		"Caroline likes painting.",
		"Caroline enjoys hiking.",
		"Caroline appreciates music.",
		"Caroline has a support network.",
		"Caroline reads books.",
		"Caroline went swimming.",
		"Caroline saw a movie.",
		"Caroline visited a museum.",
		"Caroline bought shoes.",
		"Caroline cooked dinner.",
	} {
		hits = append(hits, runners.Hit{Content: content})
	}

	body := buildAnswerBody(dataset.Question{Query: "What did Caroline research?"}, hits)
	if got := countAnswerSectionItems(body, directEvidenceSection, supportingEvidenceSection, lowerPrioritySection); got > maxDirectAnswerHits {
		t.Fatalf("direct evidence should be capped at %d, got %d:\n%s", maxDirectAnswerHits, got, body)
	}
	if got := countAnswerSectionItems(body, lowerPrioritySection); got == 0 {
		t.Fatalf("weak entity-only memories should be suppressed into lower-priority context:\n%s", body)
	}
}

func TestBuildAnswerBodyDoesNotPromoteTimestampOnlyTemporalContext(t *testing.T) {
	body := buildAnswerBody(dataset.Question{Query: "When did Melanie paint a sunrise?"}, []runners.Hit{
		{Content: "[time: 2023-05-08] Melanie shared an image of a painting of a sunrise."},
		{Content: "[time: 2023-05-08] Melanie painted a lake sunrise last year."},
		{Content: "[source_time: 2023-05-08 13:56] The weather was warm."},
		{Content: "Melanie likes pottery."},
		{Content: "Melanie likes hiking."},
		{Content: "[source_time: 2023-05-09 08:00] A receipt was shown later."},
	})

	if got := countAnswerSectionItems(body, directEvidenceSection, supportingEvidenceSection, lowerPrioritySection); got > 2 {
		t.Fatalf("temporal direct evidence should stay narrow, got %d:\n%s", got, body)
	}
	if !strings.Contains(body, lowerPrioritySection) || !strings.Contains(body, "- [#6] [source_time: 2023-05-09 08:00] A receipt was shown later.") {
		t.Fatalf("timestamp-only temporal context should be lower-priority:\n%s", body)
	}
}

func countAnswerSectionItems(body, title string, nextTitles ...string) int {
	start := strings.Index(body, title)
	if start < 0 {
		return 0
	}
	start += len(title)
	end := len(body)
	for _, next := range nextTitles {
		if idx := strings.Index(body[start:], next); idx >= 0 && start+idx < end {
			end = start + idx
		}
	}
	return strings.Count(body[start:end], "\n- [#")
}
