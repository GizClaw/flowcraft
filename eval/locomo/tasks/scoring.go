package tasks

import (
	"math"
	"regexp"
	"strings"

	"github.com/GizClaw/flowcraft/eval/locomo/dataset"
)

var tokenRE = regexp.MustCompile(`[A-Za-z0-9]+`)

func scoreQA(item dataset.QAItem, predicted string) float64 {
	predicted = strings.TrimSpace(predicted)
	if item.CategoryID == 5 || item.Category == "adversarial" {
		if isNoInfoAnswer(predicted) {
			return 1
		}
		return 0
	}
	gold := item.Answer
	if item.CategoryID == 3 || item.Category == "open-domain" {
		gold, _, _ = strings.Cut(gold, ";")
	}
	if item.CategoryID == 1 || item.Category == "multi-hop" {
		parts := splitSubAnswers(gold)
		if len(parts) > 1 {
			var sum float64
			for _, part := range parts {
				sum += tokenF1(predicted, part)
			}
			return sum / float64(len(parts))
		}
	}
	return tokenF1(predicted, gold)
}

func tokenF1(predicted, gold string) float64 {
	pred := tokens(predicted)
	ref := tokens(gold)
	if len(pred) == 0 || len(ref) == 0 {
		if len(pred) == 0 && len(ref) == 0 {
			return 1
		}
		return 0
	}
	counts := map[string]int{}
	for _, t := range ref {
		counts[t]++
	}
	var overlap int
	for _, t := range pred {
		if counts[t] > 0 {
			overlap++
			counts[t]--
		}
	}
	if overlap == 0 {
		return 0
	}
	precision := float64(overlap) / float64(len(pred))
	recall := float64(overlap) / float64(len(ref))
	return 2 * precision * recall / (precision + recall)
}

func rouge1(predicted, gold string) float64 {
	return tokenF1(predicted, gold)
}

func rougeL(predicted, gold string) float64 {
	pred := tokens(predicted)
	ref := tokens(gold)
	if len(pred) == 0 || len(ref) == 0 {
		return 0
	}
	lcs := lcsLen(pred, ref)
	if lcs == 0 {
		return 0
	}
	precision := float64(lcs) / float64(len(pred))
	recall := float64(lcs) / float64(len(ref))
	return 2 * precision * recall / (precision + recall)
}

func bleuLite(predicted, gold string) float64 {
	pred := tokens(predicted)
	ref := tokens(gold)
	if len(pred) == 0 || len(ref) == 0 {
		return 0
	}
	uni := ngramPrecision(pred, ref, 1)
	bi := ngramPrecision(pred, ref, 2)
	if bi == 0 {
		return uni * brevityPenalty(len(pred), len(ref))
	}
	return math.Sqrt(uni*bi) * brevityPenalty(len(pred), len(ref))
}

func captionTermRecall(predicted, caption string) float64 {
	capTokens := unique(tokens(caption))
	if len(capTokens) == 0 {
		return 0
	}
	predSet := unique(tokens(predicted))
	var hit int
	for t := range capTokens {
		if predSet[t] {
			hit++
		}
	}
	return float64(hit) / float64(len(capTokens))
}

func evidenceRecall(observed map[string]bool, expected []string) float64 {
	ids := NormalizeEvidenceIDs(expected)
	if len(ids) == 0 {
		return 0
	}
	var hit int
	for _, id := range ids {
		if observed[id] {
			hit++
		}
	}
	return float64(hit) / float64(len(ids))
}

// NormalizeEvidenceIDs turns LoCoMo evidence fields into atomic dialog IDs.
// Some rows encode multiple evidence IDs in one string, such as "D8:6; D9:17".
func NormalizeEvidenceIDs(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		for _, id := range splitEvidenceIDs(value) {
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func splitEvidenceIDs(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '|' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field != "" {
			out = append(out, field)
		}
	}
	return out
}

func isNoInfoAnswer(s string) bool {
	lower := strings.ToLower(s)
	for _, phrase := range []string{
		"not mentioned",
		"no information available",
		"not enough information",
		"cannot answer",
		"unknown",
		"i don't know",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func splitSubAnswers(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ';' || r == ',' || r == '|'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func tokens(s string) []string {
	raw := tokenRE.FindAllString(strings.ToLower(s), -1)
	if raw == nil {
		return []string{}
	}
	return raw
}

func unique(in []string) map[string]bool {
	out := map[string]bool{}
	for _, item := range in {
		out[item] = true
	}
	return out
}

func ngramPrecision(pred, ref []string, n int) float64 {
	if len(pred) < n || len(ref) < n {
		return 0
	}
	refCounts := ngramCounts(ref, n)
	var overlap, total int
	for gram, count := range ngramCounts(pred, n) {
		total += count
		if refCounts[gram] < count {
			overlap += refCounts[gram]
		} else {
			overlap += count
		}
	}
	if total == 0 {
		return 0
	}
	return float64(overlap) / float64(total)
}

func ngramCounts(tokens []string, n int) map[string]int {
	out := map[string]int{}
	for i := 0; i+n <= len(tokens); i++ {
		out[strings.Join(tokens[i:i+n], "\x00")]++
	}
	return out
}

func brevityPenalty(predLen, refLen int) float64 {
	if predLen == 0 {
		return 0
	}
	if predLen > refLen {
		return 1
	}
	return math.Exp(1 - float64(refLen)/float64(predLen))
}

func lcsLen(a, b []string) int {
	dp := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		prev := 0
		for j := 1; j <= len(b); j++ {
			tmp := dp[j]
			if a[i-1] == b[j-1] {
				dp[j] = prev + 1
			} else if dp[j-1] > dp[j] {
				dp[j] = dp[j-1]
			}
			prev = tmp
		}
	}
	return dp[len(b)]
}
