package locomo

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/eval/dataset"
	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// DefaultAnswerPrompt is the legacy closed-book QA prompt for runners that only
// provide flattened MEMORIES. Backends that render richer answer contexts can
// attach a format-specific PromptTemplate to runners.AnswerContext.
const DefaultAnswerPrompt = `You are answering a question using only the MEMORIES below.

Guidelines:
- Ground the answer strictly in the memories. Do not invent facts that are not supported.
- When the memories carry partial evidence that lets you reasonably infer the answer (e.g. a character's general traits, an indirectly implied date), do so and briefly note the inference. Characters whose names appear in the memories are NEVER "silent topics" — infer from their statements rather than refusing. Reply "I don't know" only when the memories are genuinely silent on the topic.
- Match the form of the question. If asked WHEN, give a specific date or duration; HOW MANY, a number; YES/NO, lead with yes/no.
- Mirror the date format used in the question (e.g. if asked "7 May 2023", answer in that format, not "May 7, 2023").
- If a memory uses a date QUALIFIER ("around", "roughly", "the week before X", "a few years ago", "last summer", "two weekends ago"), preserve that qualifier in your answer rather than computing a precise absolute date. The qualifier carries the speaker's actual epistemic state — fabricating precision is worse than mirroring vagueness.
- When an ASKED_AT line is present, treat that timestamp as the "now" for the question. Relative-time phrases ("last week", "two months ago", "yesterday", "this morning") are interpreted RELATIVE TO ASKED_AT, not to today's wall clock. Memories carry their own timestamps in the leading "[YYYY/MM/DD …]" prefix — use ASKED_AT to compute the requested window over those memory timestamps.
- Answer in 1-2 sentences. Avoid hedging ("it seems", "might be") when the memories are unambiguous.

%s

Answer:`

// buildPrediction picks between two answer strategies:
//   - opts.AnswerLLM != nil → ask the LLM to answer the question grounded in
//     the recalled memories (closed-book QA over LTM).
//   - otherwise              → cheap fallback: concatenate top-3 hits, so
//     EM/F1 still surface a "did retrieval find the right text" signal.
type answerPromptRecord struct {
	Template      string
	Body          string
	ContextFormat string
}

func buildPrediction(ctx context.Context, opts Options, q dataset.Question, artifacts []runners.RecallArtifact, answerContext runners.AnswerContext) (string, answerPromptRecord, error) {
	body := strings.TrimSpace(answerContext.Body)
	format := strings.TrimSpace(answerContext.Format)
	if body == "" {
		body = buildAnswerBody(q, artifacts)
		format = "legacy_memories"
	}
	if opts.AnswerLLM == nil {
		return composePrediction(artifacts), answerPromptRecord{Body: body, ContextFormat: format}, nil
	}
	prompt := opts.AnswerPrompt
	if prompt == "" {
		prompt = strings.TrimSpace(answerContext.PromptTemplate)
	}
	if prompt == "" {
		prompt = DefaultAnswerPrompt
	}
	fullPrompt := fmt.Sprintf(prompt, body)
	resp, _, err := opts.AnswerLLM.Generate(ctx, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: fullPrompt}}},
	})
	if err != nil {
		return "", answerPromptRecord{Template: prompt, Body: body, ContextFormat: format}, err
	}
	return strings.TrimSpace(resp.Content()), answerPromptRecord{Template: prompt, Body: body, ContextFormat: format}, nil
}

// buildAnswerBody renders the "ASKED_AT? + Q + MEMORIES" block fed into
// the QA prompt. Top-k memories are listed as bullets in RecallHit
// ranking order.
//
// The optional ASKED_AT line ([dataset.Question.AskedAt], populated by
// the LongMemEval converter from `question_date`) is emitted only when
// the source dataset records when the question was asked. Without it
// the answer LLM has no anchor for "last week" / "two months ago"
// relative-time phrases that dominate temporal-reasoning questions —
// pre-fix LongMemEval temporal-reasoning was effectively unanswerable.
// Synthetic / LoCoMo datasets that omit the field keep the legacy
// QUESTION-then-MEMORIES layout so the prompt stays stable for those
// benchmarks.
func buildAnswerBody(q dataset.Question, artifacts []runners.RecallArtifact) string {
	var b strings.Builder
	if asked := strings.TrimSpace(q.AskedAt); asked != "" {
		b.WriteString("ASKED_AT: ")
		b.WriteString(asked)
		b.WriteString("\n\n")
	}
	b.WriteString("QUESTION: ")
	b.WriteString(q.Query)
	if hints := buildAnswerHints(q.Query, artifacts); hints != "" {
		b.WriteString("\n\n")
		b.WriteString(hints)
	}
	b.WriteString("\n\nMEMORIES:\n")
	if len(artifacts) == 0 {
		b.WriteString("(none)\n")
		return b.String()
	}
	for i, h := range artifacts {
		fmt.Fprintf(&b, "- [#%d] ", i+1)
		b.WriteString(strings.ReplaceAll(h.Content, "\n", " "))
		b.WriteString("\n")
	}
	return b.String()
}

// composePrediction concatenates the top-3 hit contents — the "answer" we feed
// to EM/F1/Judge when no AnswerLLM is configured. Cheap, deterministic, and
// good enough to surface "did retrieval find the right text" without an API key.
func composePrediction(artifacts []runners.RecallArtifact) string {
	if len(artifacts) == 0 {
		return ""
	}
	max := 3
	if max > len(artifacts) {
		max = len(artifacts)
	}
	var b strings.Builder
	for i := 0; i < max; i++ {
		if i > 0 {
			b.WriteString(" || ")
		}
		b.WriteString(artifacts[i].Content)
	}
	return b.String()
}
