package locomo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/eval/dataset"
	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

type answerFakeLLM struct{}

func (answerFakeLLM) Generate(context.Context, []llm.Message, ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	return llm.NewTextMessage(llm.RoleAssistant, "answered"), llm.TokenUsage{}, nil
}

func (answerFakeLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("answerFakeLLM: streaming not implemented")
}

func TestBuildAnswerBodyAnnotatesMemoryRank(t *testing.T) {
	body := buildAnswerBody(dataset.Question{
		Query:   "When did Alice go hiking?",
		AskedAt: "2023-07-06",
	}, []runners.RecallArtifact{{
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
	assertNoAnswerCandidates(t, body)
	for _, unwanted := range []string{"kind=event", "sources=retrieval", "evidence=conv-1:D1:3"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("body should not include provenance label %q:\n%s", unwanted, body)
		}
	}
}

func TestBuildAnswerBodyNumericHints(t *testing.T) {
	body := buildAnswerBody(dataset.Question{
		Query: "How many times did Alice go to the beach?",
	}, []runners.RecallArtifact{{
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
	assertNoAnswerCandidates(t, body)
}

func TestBuildAnswerBodyListQuestionUsesMemoryFallback(t *testing.T) {
	body := buildAnswerBody(dataset.Question{
		Query: "What activities does Melanie partake in?",
	}, []runners.RecallArtifact{{
		Content: "Melanie partakes in pottery, camping, painting, and swimming.",
	}})
	assertNoAnswerCandidates(t, body)
	if !strings.Contains(body, "[#1] Melanie partakes in pottery, camping, painting, and swimming.") {
		t.Fatalf("body missing memory content:\n%s", body)
	}
}

func TestBuildAnswerBodyLiteralQuestionUsesMemoryFallback(t *testing.T) {
	body := buildAnswerBody(dataset.Question{
		Query: "What was Melanie's favorite book from her childhood?",
	}, []runners.RecallArtifact{{
		Content: `Melanie's favorite childhood book was "Charlotte's Web".`,
	}})
	assertNoAnswerCandidates(t, body)
	if !strings.Contains(body, `[#1] Melanie's favorite childhood book was "Charlotte's Web".`) {
		t.Fatalf("body missing literal memory:\n%s", body)
	}
}

func TestBuildAnswerBodyWhichQuestionDoesNotUseCandidates(t *testing.T) {
	body := buildAnswerBody(dataset.Question{
		Query: "Which US state did Jolene visit during her internship?",
	}, []runners.RecallArtifact{{
		Content: "Jolene visited Alaska during her internship.",
	}})
	assertNoAnswerCandidates(t, body)
}

func TestBuildAnswerBodyKeepsMemoriesFallbackWithoutEvidencePack(t *testing.T) {
	body := buildAnswerBody(dataset.Question{
		Query: "Did Melanie mention a class?",
	}, []runners.RecallArtifact{{
		Content: "Melanie talked about pottery class.",
	}})
	assertNoAnswerCandidates(t, body)
	if !strings.Contains(body, "MEMORIES:") || !strings.Contains(body, "[#1] Melanie talked about pottery class.") {
		t.Fatalf("memories fallback missing:\n%s", body)
	}
}

func TestBuildAnswerBodyDoesNotPromoteObservationTimeToLikelyWhen(t *testing.T) {
	body := buildAnswerBody(dataset.Question{
		Query: "When did Melanie paint a sunrise?",
	}, []runners.RecallArtifact{{
		Content: "[observed_at: 2023-05-08] | evidence: Melanie painted a lake sunrise last year. | evidence: [source_time: 2023-05-08 13:56] I painted that lake sunrise last year!",
	}})

	for _, want := range []string{
		"ANSWER_HINTS:",
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
	assertNoAnswerCandidates(t, body)
}

func TestBuildAnswerBodyKeepsPlanningRelativeHintWithoutVerifier(t *testing.T) {
	body := buildAnswerBody(dataset.Question{
		Query: "When is Melanie planning on going camping?",
	}, []runners.RecallArtifact{
		{
			Kind:    "plan",
			Content: "[observed_at: 2023-05-25] | evidence: Melanie's kids are excited about summer break and they are thinking about going camping next month. | evidence: [source_time: 2023-05-25 13:14] We're thinking about going camping next month.",
		},
		{
			Kind:    "event",
			Content: "[time: 2023-07-06] | evidence: On 2023-07-06, Melanie shared a picture of her family camping at the beach.",
		},
	})
	for _, want := range []string{
		"ANSWER_HINTS:",
		"likely_when=#2:2023-07-06",
		"relative_candidates=#1:next month",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	assertNoAnswerCandidates(t, body)
}

func TestBuildAnswerBodyLimitsWhenHintsToTopRankedMemories(t *testing.T) {
	artifacts := []runners.RecallArtifact{
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
	}, artifacts)

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
	assertNoAnswerCandidates(t, body)
}

func TestBuildAnswerBodyHidesWeakAnchorsWhenStrongEventTimeExists(t *testing.T) {
	body := buildAnswerBody(dataset.Question{
		Query: "When did Gina get her tattoo?",
	}, []runners.RecallArtifact{{
		Content: "[time: 2020-02-08] Gina got a tattoo a few years ago. | evidence: [source_time: 2023-02-08 09:32] Got the tattoo a few years ago.",
	}})

	if !strings.Contains(body, "likely_when=#1:2020-02-08") {
		t.Fatalf("body should include strong event time:\n%s", body)
	}
	for _, unwanted := range []string{"weak_observed_at=", "source_time_anchors="} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("body should hide weak anchors when strong event time exists:\n%s", body)
		}
	}
	assertNoAnswerCandidates(t, body)
}

func TestDefaultAnswerPromptMentionsRankedEvidenceAndRelativeDates(t *testing.T) {
	for _, want := range []string{
		"using only the MEMORIES below",
		"Reply \"I don't know\" only when the memories are genuinely silent",
		"date QUALIFIER",
		"leading \"[YYYY/MM/DD …]\" prefix",
	} {
		if !strings.Contains(DefaultAnswerPrompt, want) {
			t.Fatalf("DefaultAnswerPrompt missing %q", want)
		}
	}
	assertNoAnswerCandidates(t, DefaultAnswerPrompt)
}

func TestBuildPredictionUsesBackendAnswerContext(t *testing.T) {
	artifacts := []runners.RecallArtifact{{Content: "legacy fallback content"}}
	answerContext := runners.AnswerContext{
		Body:           "QUESTION: When did Alice go hiking?\n\nMEMORIES (STRUCTURED_FACTS):\n- [#1]\n  content: \"Alice went hiking.\"",
		Format:         "flowcraftv2_structured_facts",
		PromptTemplate: "backend prompt\n\n%s\n\nAnswer:",
	}

	pred, prompt, err := buildPrediction(context.Background(), Options{AnswerLLM: answerFakeLLM{}}, dataset.Question{
		Query: "When did Alice go hiking?",
	}, artifacts, answerContext)
	if err != nil {
		t.Fatalf("buildPrediction returned error: %v", err)
	}
	if pred != "answered" {
		t.Fatalf("prediction mismatch: %q", pred)
	}
	if prompt.Body != answerContext.Body {
		t.Fatalf("answer body should come from backend context:\n%s", prompt.Body)
	}
	if prompt.ContextFormat != answerContext.Format {
		t.Fatalf("context format mismatch: %q", prompt.ContextFormat)
	}
	if prompt.Template != answerContext.PromptTemplate {
		t.Fatalf("prompt template should come from backend context: %q", prompt.Template)
	}
}

func TestAnswerReplayRecordsAnswerContextFormat(t *testing.T) {
	rec := NewAnswerReplayRecord(time.Unix(0, 0).UTC(), dataset.Question{
		ID:    "q1",
		Query: "When did Alice go hiking?",
	}, nil, AnswerReplayOutcome{Prediction: "2023-07-03"}, DefaultAnswerPrompt, "body", "flowcraftv2_structured_facts")
	if rec.AnswerContextFormat != "flowcraftv2_structured_facts" {
		t.Fatalf("missing answer context format: %#v", rec)
	}
}

func assertNoAnswerCandidates(t *testing.T, text string) {
	t.Helper()
	for _, unwanted := range []string{"ANSWER_CANDIDATES", "PREFERRED_ANSWER_CANDIDATE", "REJECTED_CANDIDATES"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("unexpected candidate marker %q:\n%s", unwanted, text)
		}
	}
}
