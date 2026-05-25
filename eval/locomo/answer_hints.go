package locomo

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
)

var (
	answerHintTimeTagRE     = regexp.MustCompile(`\[time:\s*([0-9]{4}-[0-9]{2}-[0-9]{2})\]`)
	answerHintObservedAtRE  = regexp.MustCompile(`\[observed_at:\s*([0-9]{4}-[0-9]{2}-[0-9]{2})\]`)
	answerHintSourceTimeRE  = regexp.MustCompile(`\[source_time:\s*([0-9]{4}-[0-9]{2}-[0-9]{2})(?:\s+[0-9]{2}:[0-9]{2})?\]`)
	answerHintDateRE        = regexp.MustCompile(`\b(?:[0-9]{4}-[0-9]{2}-[0-9]{2}|[0-9]{1,2}\s+[A-Za-z]+\s+[0-9]{4}|[A-Za-z]+\s+[0-9]{1,2},?\s+[0-9]{4})\b`)
	answerHintRelativeRE    = regexp.MustCompile(`(?i)\b(?:the\s+)?(?:day|week|weekend|month|year|summer|winter|spring|fall|autumn)\s+(?:before|after)\s+[^.|;,\]]+`)
	answerHintNumericRE     = regexp.MustCompile(`(?i)(?:[$€£]\s*\d+(?:[.,]\d+)?|\d+(?:[.,]\d+)?\s*(?:%|percent|percentage|minutes?|hours?|days?|weeks?|months?|years?|times?|x)\b|\b\d+(?:[.,]\d+)?\b)`)
	answerHintQuestionSpace = regexp.MustCompile(`\s+`)
)

func buildAnswerHints(query string, hits []runners.Hit) string {
	kind := answerHintKind(query)
	if kind == "" || len(hits) == 0 {
		return ""
	}
	switch kind {
	case "when":
		return buildWhenAnswerHints(hits)
	case "numeric":
		return buildNumericAnswerHints(hits)
	default:
		return ""
	}
}

func answerHintKind(query string) string {
	q := strings.ToLower(answerHintQuestionSpace.ReplaceAllString(strings.TrimSpace(query), " "))
	switch {
	case strings.HasPrefix(q, "when ") ||
		strings.HasPrefix(q, "what date ") ||
		strings.HasPrefix(q, "what day ") ||
		strings.HasPrefix(q, "which day ") ||
		strings.HasPrefix(q, "what month ") ||
		strings.HasPrefix(q, "which month ") ||
		strings.HasPrefix(q, "what year ") ||
		strings.HasPrefix(q, "which year "):
		return "when"
	case strings.HasPrefix(q, "how many ") ||
		strings.HasPrefix(q, "how much ") ||
		strings.HasPrefix(q, "how long ") ||
		strings.HasPrefix(q, "how often "):
		return "numeric"
	default:
		return ""
	}
}

func buildWhenAnswerHints(hits []runners.Hit) string {
	var eventTimes, relative, observed, sourceTimes []string
	for rank, hit := range hits {
		content := hit.Content
		eventTimes = appendUniqueHint(eventTimes, rankedHint(rank, firstSubmatch(answerHintTimeTagRE, content)))
		relative = appendUniqueHint(relative, rankedHint(rank, firstMatch(answerHintRelativeRE, content)))
		if len(eventTimes) == 0 {
			eventTimes = appendUniqueHint(eventTimes, rankedHint(rank, firstMatch(answerHintDateRE, content)))
		}
		observed = appendUniqueHint(observed, rankedHint(rank, firstSubmatch(answerHintObservedAtRE, content)))
		sourceTimes = appendUniqueHint(sourceTimes, rankedHint(rank, firstSubmatch(answerHintSourceTimeRE, content)))
	}
	if len(eventTimes) == 0 && len(relative) == 0 && len(observed) == 0 && len(sourceTimes) == 0 {
		return ""
	}
	parts := []string{}
	if len(eventTimes) > 0 {
		parts = append(parts, "likely_when="+strings.Join(eventTimes, "; "))
	}
	if len(relative) > 0 {
		parts = append(parts, "relative_candidates="+strings.Join(relative, "; "))
	}
	if len(observed) > 0 {
		parts = append(parts, "weak_observed_at="+strings.Join(observed, "; "))
	}
	if len(sourceTimes) > 0 {
		parts = append(parts, "source_time_anchors="+strings.Join(sourceTimes, "; "))
	}
	parts = append(parts, "rule=prefer [time:] and explicit relative wording; do not recompute a relative phrase on top of [time:]")
	return "ANSWER_HINTS: " + strings.Join(parts, " | ")
}

func buildNumericAnswerHints(hits []runners.Hit) string {
	var candidates []string
	for rank, hit := range hits {
		for _, raw := range answerHintNumericRE.FindAllString(hit.Content, 6) {
			candidates = appendUniqueHint(candidates, rankedHint(rank, raw))
			if len(candidates) >= 8 {
				break
			}
		}
		if len(candidates) >= 8 {
			break
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	return "ANSWER_HINTS: numeric_candidates=" + strings.Join(candidates, "; ") + " | rule=choose the candidate that matches the question's numeric slot and top-ranked supporting memory"
}

func firstSubmatch(re *regexp.Regexp, text string) string {
	m := re.FindStringSubmatch(text)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func firstMatch(re *regexp.Regexp, text string) string {
	return strings.TrimSpace(re.FindString(text))
}

func rankedHint(rank int, text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return fmt.Sprintf("#%d:%s", rank+1, text)
}

func appendUniqueHint(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	key := strings.ToLower(value)
	for _, existing := range values {
		if strings.ToLower(existing) == key {
			return values
		}
	}
	return append(values, value)
}
