package main

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

// scoreEMF1 computes loose EM (gold contained in prediction) and token F1
// against every gold answer, keeping the best F1.
func scoreEMF1(prediction string, golds []string) (float64, float64) {
	normalizedPrediction := normalizeText(prediction)
	em := 0.0
	bestF1 := 0.0
	for _, gold := range golds {
		normalizedGold := normalizeText(gold)
		if normalizedGold != "" && strings.Contains(normalizedPrediction, normalizedGold) {
			em = 1
		}
		f1 := tokenF1(normalizedPrediction, normalizedGold)
		if f1 > bestF1 {
			bestF1 = f1
		}
	}
	return em, bestF1
}

func normalizeText(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}

func tokenF1(prediction, gold string) float64 {
	predictionTokens := tokenize(prediction)
	goldTokens := tokenize(gold)
	if len(goldTokens) == 0 {
		return 0
	}
	if len(predictionTokens) == 0 {
		return 0
	}
	goldSet := make(map[string]struct{}, len(goldTokens))
	for _, token := range goldTokens {
		goldSet[token] = struct{}{}
	}
	matched := 0
	for _, token := range predictionTokens {
		if _, ok := goldSet[token]; ok {
			matched++
		}
	}
	if matched == 0 {
		return 0
	}
	precision := float64(matched) / float64(len(predictionTokens))
	recall := float64(matched) / float64(len(goldTokens))
	return 2 * precision * recall / (precision + recall)
}

func tokenize(value string) []string {
	return strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// judgeResponse asks the judge model whether the prediction answers the gold.
func judgeResponse(
	ctx context.Context,
	runtime *inference.Runtime,
	model inference.ModelRef,
	golds []string,
	prediction string,
) (float64, error) {
	var prompt strings.Builder
	prompt.WriteString("Decide whether the prediction answers the gold answer(s) correctly.\n")
	prompt.WriteString("Answer with exactly one word: CORRECT or INCORRECT.\n\n")
	prompt.WriteString("Gold:\n")
	for _, gold := range golds {
		fmt.Fprintf(&prompt, "- %s\n", gold)
	}
	fmt.Fprintf(&prompt, "\nPrediction:\n%s\n", prediction)
	response, err := generateText(ctx, runtime, model, prompt.String())
	if err != nil {
		return 0, err
	}
	lower := strings.ToLower(strings.TrimSpace(response))
	if strings.Contains(lower, "incorrect") {
		return 0, nil
	}
	if strings.Contains(lower, "correct") {
		return 1, nil
	}
	return 0, fmt.Errorf("judge response %q is not CORRECT or INCORRECT", response)
}
