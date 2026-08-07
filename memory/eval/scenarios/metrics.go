package scenarios

import (
	"strings"
	"unicode"
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
	if len(goldTokens) == 0 || len(predictionTokens) == 0 {
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
