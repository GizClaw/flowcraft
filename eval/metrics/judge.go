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
type LLMJudge struct {
	LLM    llm.LLM
	Prompt string // %s receives "Q: …\nGold: …\nPrediction: …"; default below
}

// DefaultLLMJudgePrompt is the prompt used when LLMJudge.Prompt is empty.
const DefaultLLMJudgePrompt = `You are an evaluator. Given the QUESTION, the GOLD answers, and a PREDICTION, decide if the prediction is semantically correct.

%s

Output JSON: {"correct": true} if the prediction matches one of the gold answers (semantic equivalence is fine), otherwise {"correct": false}.`

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
	body := fmt.Sprintf("Q: %s\nGOLD: %s\nPREDICTION: %s", q, strings.Join(golds, " | "), pred)
	resp, _, err := l.LLM.Generate(ctx, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: fmt.Sprintf(prompt, body)}}},
	},
		llm.WithJSONSchema(judgeSchema),
		llm.WithJSONMode(true),
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
