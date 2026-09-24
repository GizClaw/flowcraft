// Package official implements the LoCoMo leaderboard metric (token F1 with
// Porter stemming, plus the category rules from the reference harness), so our
// numbers can be compared with published ones instead of only with our own
// history. It is a pure scorer: no model calls, so a stored report can be
// re-scored for free.
package official

import (
	"strings"

	"github.com/reiver/go-porterstemmer"
)

// asciiPunctuation is string.punctuation from the reference implementation.
const asciiPunctuation = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"

// normalizeAnswer mirrors normalize_answer from the reference harness exactly:
//
//	s = s.replace(',', "")
//	remove_punc: delete ASCII punctuation (it is dropped, not split)
//	remove_articles: a, an, the, and
//	lower, then collapse whitespace
//
// The deletion matters: it is what makes "Caroline's" one token ("carolines")
// rather than two, and "1,000" one token ("1000"). Replacing punctuation with a
// space instead -- which this scorer used to do -- quietly scored every
// possessive, hyphenated name and grouped number differently from the
// published numbers.
func normalizeAnswer(value string) string {
	lowered := strings.ToLower(value)
	lowered = strings.ReplaceAll(lowered, ",", "")
	var builder strings.Builder
	builder.Grow(len(lowered))
	for _, r := range lowered {
		if r < 128 && strings.ContainsRune(asciiPunctuation, r) {
			continue
		}
		builder.WriteRune(r)
	}
	fields := strings.Fields(builder.String())
	kept := fields[:0]
	for _, field := range fields {
		switch field {
		case "a", "an", "the", "and":
			continue
		}
		kept = append(kept, field)
	}
	return strings.Join(kept, " ")
}

// Normalize is what F1 scores on: normalize_answer followed by Porter stemming
// of every token, which is the order the reference harness uses.
//
// One divergence remains and is deliberate: the reference stems with NLTK's
// PorterStemmer, this uses reiver/go-porterstemmer. They agree on ordinary
// words but not on every edge case ("may" is one we have measured), so treat a
// difference in the last decimal as a stemmer artefact rather than a result.
func Normalize(value string) string {
	fields := strings.Fields(normalizeAnswer(value))
	kept := fields[:0]
	for _, field := range fields {
		kept = append(kept, porterstemmer.StemString(field))
	}
	return strings.Join(kept, " ")
}

// F1 is the token-level F1 used by the reference harness (stemmed, multiset
// overlap).
func F1(prediction, gold string) float64 {
	predicted := strings.Fields(Normalize(prediction))
	expected := strings.Fields(Normalize(gold))
	if len(predicted) == 0 || len(expected) == 0 {
		return 0
	}
	common := 0
	used := make([]bool, len(expected))
	for _, token := range predicted {
		for index, want := range expected {
			if used[index] || token != want {
				continue
			}
			used[index] = true
			common++
			break
		}
	}
	if common == 0 {
		return 0
	}
	precision := float64(common) / float64(len(predicted))
	recall := float64(common) / float64(len(expected))
	return 2 * precision * recall / (precision + recall)
}

// Score applies the category rules of the reference harness:
//
//	category 1 (multi-hop): comma-split answers, mean of the best F1 per gold
//	category 2, 4:           plain F1
//	category 3 (open domain): plain F1 against the first ";" segment of the gold
//	category 5 (adversarial): 1 when the prediction refuses, 0 otherwise
func Score(category int, prediction string, golds []string) float64 {
	cleaned := make([]string, 0, len(golds))
	for _, gold := range golds {
		if trimmed := strings.TrimSpace(gold); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	switch category {
	case 5:
		lowered := strings.ToLower(prediction)
		if strings.Contains(lowered, "no information available") || strings.Contains(lowered, "not mentioned") {
			return 1
		}
		return 0
	case 1:
		return multiHopF1(prediction, cleaned)
	case 3:
		best := 0.0
		for _, gold := range cleaned {
			head, _, _ := strings.Cut(gold, ";")
			if score := F1(prediction, strings.TrimSpace(head)); score > best {
				best = score
			}
		}
		return best
	default:
		best := 0.0
		for _, gold := range cleaned {
			if score := F1(prediction, gold); score > best {
				best = score
			}
		}
		return best
	}
}

// multiHopF1 splits both sides on commas and averages the best score of every
// gold sub-answer, which is how the reference harness scores category 1: a
// prediction that lists one of two items scores 0.5, not 1.
func multiHopF1(prediction string, golds []string) float64 {
	if len(golds) == 0 {
		return 0
	}
	predictions := splitTrimmed(prediction)
	if len(predictions) == 0 {
		predictions = []string{prediction}
	}
	best := 0.0
	for _, gold := range golds {
		parts := splitTrimmed(gold)
		if len(parts) == 0 {
			continue
		}
		total := 0.0
		for _, want := range parts {
			subBest := 0.0
			for _, candidate := range predictions {
				if score := F1(candidate, want); score > subBest {
					subBest = score
				}
			}
			total += subBest
		}
		if mean := total / float64(len(parts)); mean > best {
			best = mean
		}
	}
	return best
}

func splitTrimmed(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
