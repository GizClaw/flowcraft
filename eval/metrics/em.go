// Package metrics implements the eval indicators specified in.
package metrics

import (
	"sort"
	"strings"
	"unicode"
)

// Normalize collapses case + whitespace + punctuation for EM/F1.
func Normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '\u4e00' || (r >= 0x3400 && r <= 0x9fff):
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// ExactMatch returns true iff any gold normalized substring is contained in the
// normalized prediction (loose EM, the convention used by LongMemEval).
func ExactMatch(prediction string, golds []string) bool {
	pred := Normalize(prediction)
	if pred == "" {
		return false
	}
	for _, g := range golds {
		gn := Normalize(g)
		if gn == "" {
			continue
		}
		if strings.Contains(pred, gn) {
			return true
		}
	}
	return false
}

// F1 token-overlap score against the best-matching gold.
func F1(prediction string, golds []string) float64 {
	p := strings.Fields(Normalize(prediction))
	if len(p) == 0 {
		return 0
	}
	best := 0.0
	for _, g := range golds {
		ref := strings.Fields(Normalize(g))
		if len(ref) == 0 {
			continue
		}
		commons := tokenOverlap(p, ref)
		if commons == 0 {
			continue
		}
		prec := float64(commons) / float64(len(p))
		rec := float64(commons) / float64(len(ref))
		f := 2 * prec * rec / (prec + rec)
		if f > best {
			best = f
		}
	}
	return best
}

func tokenOverlap(a, b []string) int {
	bag := map[string]int{}
	for _, t := range b {
		bag[t]++
	}
	count := 0
	for _, t := range a {
		if bag[t] > 0 {
			count++
			bag[t]--
		}
	}
	return count
}

// SortedKeys is exposed for stable JSON output.
func SortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
