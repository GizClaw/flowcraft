package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/dataset"
	locomoreport "github.com/GizClaw/flowcraft/eval/locomo/report"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func judgeQA(ctx context.Context, judge llm.LLM, item dataset.QAItem, predicted string, timeout time.Duration) *locomoreport.QAJudgeResult {
	if judge == nil {
		return nil
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	msg, _, err := judge.Generate(ctx, qaJudgeMessages(item, predicted), llm.WithJSONMode(true))
	if err != nil {
		return &locomoreport.QAJudgeResult{Error: err.Error()}
	}
	result, err := parseQAJudgeResult(msg.Content(), item)
	if err != nil {
		return &locomoreport.QAJudgeResult{Error: err.Error()}
	}
	return result
}

func qaJudgeMessages(item dataset.QAItem, predicted string) []llm.Message {
	return []llm.Message{
		model.NewTextMessage(model.RoleSystem, qaJudgeSystemPrompt()),
		model.NewTextMessage(model.RoleUser, qaJudgeUserPrompt(item, predicted)),
	}
}

func qaJudgeSystemPrompt() string {
	return `You are grading a memory QA evaluation answer.

Return only JSON with this shape:
{
  "verdict": "correct|incorrect",
  "rationale": "brief reason"
}

Grading rules:
- correct: predicted answer semantically answers the question and matches the gold answer.
- incorrect: predicted answer is wrong, unsupported, incomplete, less specific than the gold answer, or says no information when the gold answer is answerable.
- Treat paraphrases, harmless wording differences, articles, capitalization, and minor spelling mistakes as correct when the meaning is clear.
- Treat a predicted answer that is more specific than the gold answer as correct if the extra specificity does not contradict the question or gold answer.
- For dates and times, accept equivalent granularity or a more specific date inside the same stated time range; reject contradictory day, month, year, sequence, or count.
- For lists, accept the answer when it includes the required gold items and any extra items do not contradict the question. Reject answers that omit required items or add conflicting items.
- For adversarial/unanswerable questions with an empty gold answer, correct means the predicted answer explicitly refuses, says no information, or says there is not enough information. Any answer that borrows facts from another person or subject is incorrect.
- For adversarial questions with a non-empty gold answer, correct means the predicted answer semantically matches that gold answer.
- Unless Category is "adversarial" or Category ID is 5, the gold answer is answerable; a "no information" prediction must be incorrect.
- Do not require exact wording, and do not mark an answer incorrect only because it includes a non-conflicting detail.`
}

func qaJudgeUserPrompt(item dataset.QAItem, predicted string) string {
	return `Question: ` + item.Question + `
Category: ` + item.Category + `
Category ID: ` + fmt.Sprint(item.CategoryID) + `
Gold answer: ` + item.Answer + `
Predicted answer: ` + predicted
}

func parseQAJudgeResult(content string, item dataset.QAItem) (*locomoreport.QAJudgeResult, error) {
	var raw struct {
		Verdict   string `json:"verdict"`
		Rationale string `json:"rationale"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &raw); err != nil {
		return nil, fmt.Errorf("locomo tasks: invalid QA judge JSON: %w", err)
	}
	verdict := normalizeBinaryJudgeVerdict(raw.Verdict, item)
	if verdict == "" {
		return nil, fmt.Errorf("locomo tasks: invalid QA judge verdict %q", raw.Verdict)
	}
	return &locomoreport.QAJudgeResult{
		Verdict:   verdict,
		Correct:   verdict == "correct",
		Rationale: strings.TrimSpace(raw.Rationale),
	}, nil
}

func isAdversarialQA(item dataset.QAItem) bool {
	return item.CategoryID == 5 || strings.EqualFold(strings.TrimSpace(item.Category), "adversarial")
}

func normalizeBinaryJudgeVerdict(verdict string, item dataset.QAItem) string {
	switch strings.TrimSpace(strings.ToLower(verdict)) {
	case "correct":
		return "correct"
	case "incorrect", "wrong":
		return "incorrect"
	case "no_info_correct", "no info correct", "no-information-correct", "unanswerable_correct":
		if isAdversarialQA(item) {
			return "correct"
		}
		return "incorrect"
	case "partial", "partially_correct", "partially correct":
		return "incorrect"
	default:
		return ""
	}
}
