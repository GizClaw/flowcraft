package locomo

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

const (
	directEvidenceSection     = "DIRECT EVIDENCE (answer from these first):"
	supportingEvidenceSection = "SUPPORTING EVIDENCE (use to complete lists or resolve ambiguity):"
	lowerPrioritySection      = "LOWER-PRIORITY CONTEXT (possible distractors; use only if needed):"
)

type answerQueryFeatures struct {
	tokens        map[string]struct{}
	numeric       map[string]struct{}
	temporal      bool
	numericIntent bool
	listIntent    bool
}

type organizedAnswerHit struct {
	rank  int
	hit   runners.Hit
	score float64
}

type organizedAnswerContext struct {
	direct     []organizedAnswerHit
	supporting []organizedAnswerHit
	lower      []organizedAnswerHit
}

func renderOrganizedAnswerMemories(query string, hits []runners.Hit) string {
	if len(hits) == 0 {
		return "(none)\n"
	}
	ctx := organizeAnswerContext(query, hits)
	var b strings.Builder
	renderSection := func(title string, section []organizedAnswerHit) {
		if len(section) == 0 {
			return
		}
		b.WriteString(title)
		b.WriteByte('\n')
		for _, item := range section {
			fmt.Fprintf(&b, "- [#%d] ", item.rank)
			b.WriteString(strings.ReplaceAll(item.hit.Content, "\n", " "))
			b.WriteByte('\n')
		}
	}
	renderSection(directEvidenceSection, ctx.direct)
	renderSection(supportingEvidenceSection, ctx.supporting)
	renderSection(lowerPrioritySection, ctx.lower)
	return b.String()
}

func organizeAnswerContext(query string, hits []runners.Hit) organizedAnswerContext {
	features := newAnswerQueryFeatures(query)
	out := organizedAnswerContext{}
	for i, hit := range hits {
		item := organizedAnswerHit{rank: i + 1, hit: hit}
		item.score = answerHitRelevance(features, hit.Content)
		switch {
		case answerHitIsDirect(features, item):
			out.direct = append(out.direct, item)
		case answerHitIsSupporting(features, item):
			out.supporting = append(out.supporting, item)
		default:
			out.lower = append(out.lower, item)
		}
	}
	if len(out.direct) == 0 {
		promote := 3
		if promote > len(out.lower) {
			promote = len(out.lower)
		}
		out.direct = append(out.direct, out.lower[:promote]...)
		out.lower = out.lower[promote:]
	}
	return out
}

func newAnswerQueryFeatures(query string) answerQueryFeatures {
	return answerQueryFeatures{
		tokens:        answerTokenSet(query),
		numeric:       answerNumericTokens(query),
		temporal:      answerHasTemporalIntent(query),
		numericIntent: answerHasNumericIntent(query),
		listIntent:    answerHasListIntent(query),
	}
}

func answerHitIsDirect(query answerQueryFeatures, item organizedAnswerHit) bool {
	if item.rank <= 2 {
		return true
	}
	if item.score >= 0.34 {
		return true
	}
	if query.temporal && item.rank <= 8 && answerHasTimeSignal(item.hit.Content) && item.score >= 0.12 {
		return true
	}
	if query.numericIntent && item.rank <= 8 && answerNumericOverlap(query.numeric, item.hit.Content) {
		return true
	}
	return false
}

func answerHitIsSupporting(query answerQueryFeatures, item organizedAnswerHit) bool {
	if item.rank <= 5 {
		return true
	}
	if item.score >= 0.18 {
		return true
	}
	if query.listIntent && item.rank <= 15 && item.score > 0 {
		return true
	}
	if query.temporal && item.rank <= 15 && answerHasTimeSignal(item.hit.Content) && item.score > 0 {
		return true
	}
	if query.numericIntent && item.rank <= 15 && answerNumericOverlap(query.numeric, item.hit.Content) {
		return true
	}
	return false
}

func answerHitRelevance(query answerQueryFeatures, text string) float64 {
	if len(query.tokens) == 0 {
		return 0
	}
	tokens := answerTokenSet(text)
	if len(tokens) == 0 {
		return 0
	}
	matched := 0
	for tok := range query.tokens {
		if _, ok := tokens[tok]; ok {
			matched++
		}
	}
	score := float64(matched) / float64(len(query.tokens))
	if query.temporal && answerHasTimeSignal(text) {
		score += 0.10
	}
	if query.numericIntent && answerNumericOverlap(query.numeric, text) {
		score += 0.10
	}
	if score > 1 {
		return 1
	}
	return score
}

func answerTokenSet(text string) map[string]struct{} {
	return stringSet(tokenize.Detect(text).Tokenize(text))
}

func stringSet(tokens []string) map[string]struct{} {
	out := make(map[string]struct{}, len(tokens))
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		out[tok] = struct{}{}
	}
	return out
}

func answerNumericTokens(text string) map[string]struct{} {
	out := map[string]struct{}{}
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		out[cur.String()] = struct{}{}
		cur.Reset()
	}
	for _, r := range text {
		if unicode.IsDigit(r) || unicode.IsNumber(r) {
			cur.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

func answerNumericOverlap(queryNumbers map[string]struct{}, text string) bool {
	if len(queryNumbers) == 0 {
		return false
	}
	for n := range answerNumericTokens(text) {
		if _, ok := queryNumbers[n]; ok {
			return true
		}
	}
	return false
}

var answerDateLikeRE = regexp.MustCompile(`(?i)\b(?:\d{4}-\d{1,2}-\d{1,2}|\d{1,2}[/-]\d{1,2}[/-]\d{2,4}|jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\b`)

func answerHasTimeSignal(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "[time:") ||
		strings.Contains(lower, "[source_time:") ||
		answerDateLikeRE.MatchString(text)
}

func answerHasTemporalIntent(query string) bool {
	q := strings.ToLower(query)
	for _, cue := range []string{
		"when", "what date", "which day", "how long", "how old",
		"before", "after", "during", "last week", "next month",
	} {
		if strings.Contains(q, cue) {
			return true
		}
	}
	return answerDateLikeRE.MatchString(query)
}

func answerHasNumericIntent(query string) bool {
	q := strings.ToLower(query)
	for _, cue := range []string{"how many", "how much", "number", "count", "total"} {
		if strings.Contains(q, cue) {
			return true
		}
	}
	return false
}

func answerHasListIntent(query string) bool {
	q := strings.ToLower(query)
	for _, cue := range []string{
		"what are", "which are", "list", "types", "kinds", "activities",
		"items", "books", "movies", "songs", "cities", "countries", "names",
	} {
		if strings.Contains(q, cue) {
			return true
		}
	}
	return false
}
