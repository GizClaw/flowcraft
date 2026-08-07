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

// scoreItemRecall measures how much of the gold answer's item list the
// prediction covers, keeping the best coverage across gold answers. Items
// are split on commas, semicolons, and " and " so enumerative golds such as
// "pottery, camping, painting" are scored per item; a prediction that names
// some but not all items gets partial credit instead of 0.
func scoreItemRecall(prediction string, golds []string) float64 {
	predictionTokens := tokenSet(prediction)
	best := 0.0
	for _, gold := range golds {
		items := splitGoldItems(gold)
		if len(items) == 0 {
			continue
		}
		matched := 0
		for _, item := range items {
			covered := true
			for token := range tokenSet(item) {
				if !predictionTokens[token] {
					covered = false
					break
				}
			}
			if covered {
				matched++
			}
		}
		recall := float64(matched) / float64(len(items))
		if recall > best {
			best = recall
		}
	}
	return best
}

// splitGoldItems splits one gold answer into concrete items. A single
// entity ("San Francisco", "2022") stays one item, while enumerative
// answers ("pottery, camping, painting", "Nils Frahm and Olafur Arnalds")
// become one item per entry.
func splitGoldItems(gold string) []string {
	normalized := normalizeText(gold)
	if normalized == "" {
		return nil
	}
	parts := strings.FieldsFunc(normalized, func(r rune) bool {
		return r == ',' || r == ';' || r == '，' || r == '；' || r == '&'
	})
	var items []string
	seen := make(map[string]bool)
	for _, part := range parts {
		for _, sub := range strings.Split(part, " and ") {
			sub = strings.TrimSpace(sub)
			if sub == "" || seen[sub] {
				continue
			}
			seen[sub] = true
			items = append(items, sub)
		}
	}
	return items
}

func tokenSet(value string) map[string]bool {
	tokens := tokenize(value)
	set := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		set[token] = true
	}
	return set
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
