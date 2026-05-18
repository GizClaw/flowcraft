package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// Judge scores a (question, prediction, golds) tuple in [0,1].
type Judge interface {
	Score(ctx context.Context, question, prediction string, golds []string) (float64, error)
}

// EMJudge is a deterministic judge that returns 1.0 iff EM matches; useful for
// CI runs without an LLM budget.
type EMJudge struct{}

// Score implements Judge.
func (EMJudge) Score(_ context.Context, _ string, prediction string, golds []string) (float64, error) {
	if ExactMatch(prediction, golds) {
		return 1, nil
	}
	return F1(prediction, golds), nil
}

// LLMJudge prompts an LLM with the gold/prediction pair and returns 1.0/0.0
// based on a structured judgement (the qa.judge metric).
//
// The model is constrained to a strict {"correct": true|false} JSON-Schema so
// formatting drift ("Yes.", "Correct", "✓") can't degrade the score.
//
// Temperature defaults to 0 (deterministic) when nil. Set it explicitly to
// override.
type LLMJudge struct {
	LLM         llm.LLM
	Prompt      string   // %s receives "Q: …\nGold: …\nPrediction: …"; default below
	Temperature *float64 // nil → 0 (deterministic)
}

// DefaultLLMJudgePrompt is FlowCraft's default answer-inclusion judge.
// It marks a prediction correct when it conveys the gold answer's core
// information, including as a paraphrase or inside a longer sentence with
// additional context. It deliberately rejects topic-only matches and
// ungrounded relative-time answers, which makes it less strict than exact
// semantic equivalence but less permissive than the LoCoMo leaderboard prompt.
const DefaultLLMJudgePrompt = `You are an evaluator. Given the QUESTION, the GOLD answers, and a PREDICTION, decide whether the prediction includes the core information from at least one gold answer.

%s

Mark {"correct": true} when the prediction states the gold answer's core information, either verbatim, as a paraphrase, or as part of a longer sentence with additional supported context.

Mark {"correct": false} when:
- the prediction merely touches on the same topic without stating the gold information;
- the prediction contradicts the gold answer;
- the prediction uses a relative time reference that cannot be grounded to the gold answer's absolute date or the provided conversation context.

Formatting differences are fine: "May 7th" and "7 May" can match when they refer to the same date.

Output strict JSON only: {"correct": true} or {"correct": false}.`

// LocoMoLLMJudgePrompt mirrors the prompt used by common LoCoMo leaderboard
// harnesses. It is opt-in only; use it when reproducing published LoCoMo
// comparison numbers, not as FlowCraft's default judge semantics.
//
// Three substantive differences from DefaultLLMJudgePrompt:
//   - explicit "be generous" instruction (topic-level match counts as CORRECT)
//   - explicit date-format leniency for temporal questions
//   - asks for a one-sentence reasoning step before the verdict (light CoT)
//
// These three tweaks together typically lift qa.judge by ~3-5pp on the same
// underlying predictions; the gain is methodology alignment, not framework
// improvement.
const LocoMoLLMJudgePrompt = `Your task is to label an answer to a question as 'CORRECT' or 'WRONG'. You will be given the following data:
  (1) a question (posed by one user to another user),
  (2) a 'gold' (ground truth) answer,
  (3) a generated answer
which you will score as CORRECT/WRONG.

The point of the question is to ask about something one user should know about the other user based on their prior conversations.
The gold answer will usually be a concise and short answer that includes the referenced topic, for example:
Question: Do you remember what I got the last time I went to Hawaii?
Gold answer: A shell necklace
The generated answer might be much longer, but you should be generous with your grading - as long as it touches on the same topic as the gold answer, it should be counted as CORRECT.

For time related questions, the gold answer will be a specific date, month, year, etc. The generated answer might be much longer or use relative time references (like "last Tuesday" or "next month"), but you should be generous with your grading - as long as it refers to the same date or time period as the gold answer, it should be counted as CORRECT. Even if the format differs (e.g., "May 7th" vs "7 May"), consider it CORRECT if it's the same date.

Now it's time for the real question:
%s

First, provide a short (one sentence) explanation of your reasoning, then return your verdict.

Return JSON: {"correct": true} if CORRECT, otherwise {"correct": false}.`

var judgeSchema = llm.JSONSchemaParam{
	Name:        "qa_judgement",
	Description: "Whether a candidate answer matches the gold answer(s)",
	Strict:      true,
	Schema: map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"correct"},
		"properties": map[string]any{
			"correct": map[string]any{"type": "boolean"},
		},
	},
}

// Score implements Judge.
func (l LLMJudge) Score(ctx context.Context, q, pred string, golds []string) (float64, error) {
	if l.LLM == nil {
		return 0, errors.New("locomo: LLMJudge.LLM is required")
	}
	prompt := l.Prompt
	if prompt == "" {
		prompt = DefaultLLMJudgePrompt
	}
	body := fmt.Sprintf("Question: %s\nGold answer: %s\nGenerated answer: %s", q, strings.Join(golds, " | "), pred)
	temp := 0.0
	if l.Temperature != nil {
		temp = *l.Temperature
	}
	resp, _, err := l.LLM.Generate(ctx, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: fmt.Sprintf(prompt, body)}}},
	},
		llm.WithJSONSchema(judgeSchema),
		llm.WithJSONMode(true),
		llm.WithTemperature(temp),
	)
	if err != nil {
		return 0, err
	}
	payload, _, err := llm.ExtractJSON(resp.Content())
	if err != nil {
		// Fallback: textual yes/no parse for providers that ignored the schema.
		out := strings.ToLower(strings.TrimSpace(resp.Content()))
		if strings.HasPrefix(out, "yes") || strings.Contains(out, `"correct": true`) || strings.Contains(out, `"correct":true`) {
			return 1, nil
		}
		return 0, nil
	}
	var v struct {
		Correct bool `json:"correct"`
	}
	if err := json.Unmarshal(payload, &v); err != nil {
		return 0, nil
	}
	if v.Correct {
		return 1, nil
	}
	return 0, nil
}
