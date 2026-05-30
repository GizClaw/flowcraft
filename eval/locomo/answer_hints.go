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
	answerHintRelativeRE    = regexp.MustCompile(`(?i)\b(?:(?:the\s+)?(?:day|week|weekend|month|year|summer|winter|spring|fall|autumn)\s+(?:before|after)\s+[^.|;,\]]+|(?:last|next|this)\s+(?:day|week|weekend|month|year|summer|winter|spring|fall|autumn)|(?:a|an|one|two|three|four|five|six|seven|eight|nine|ten|\d+)\s+(?:days?|weeks?|months?|years?)\s+ago)\b`)
	answerHintNumericRE     = regexp.MustCompile(`(?i)(?:[$€£]\s*\d+(?:[.,]\d+)?|\d+(?:[.,]\d+)?\s*(?:%|percent|percentage|minutes?|hours?|days?|weeks?|months?|years?|times?|x)\b|\b\d+(?:[.,]\d+)?\b)`)
	answerHintQuestionSpace = regexp.MustCompile(`\s+`)
)

const (
	answerHintStrongRankLimit = 8
	answerHintWeakRankLimit   = 3
	answerHintMaxValues       = 8
	answerHintMaxWeakValues   = 3
)

func buildAnswerHints(query string, artifacts []runners.RecallArtifact) string {
	kind := answerHintKind(query)
	if kind == "" || len(artifacts) == 0 {
		return ""
	}
	switch kind {
	case "when":
		return buildWhenAnswerHints(artifacts)
	case "numeric":
		return buildNumericAnswerHints(artifacts)
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

func buildWhenAnswerHints(artifacts []runners.RecallArtifact) string {
	var eventTimes, relative, observed, sourceTimes []string
	for rank, artifact := range artifacts {
		content := artifact.Content
		if rank < answerHintStrongRankLimit {
			dateSearchText := answerHintDateSearchText(content)
			eventTimes = appendUniqueHintLimited(eventTimes, rankedHint(rank, firstSubmatch(answerHintTimeTagRE, content)), answerHintMaxValues)
			relative = appendUniqueHintLimited(relative, rankedHint(rank, firstMatch(answerHintRelativeRE, content)), answerHintMaxValues)
			if len(eventTimes) == 0 {
				eventTimes = appendUniqueHintLimited(eventTimes, rankedHint(rank, firstMatch(answerHintDateRE, dateSearchText)), answerHintMaxValues)
			}
		}
		if rank < answerHintWeakRankLimit {
			observed = appendUniqueHintLimited(observed, rankedHint(rank, firstSubmatch(answerHintObservedAtRE, content)), answerHintMaxWeakValues)
			sourceTimes = appendUniqueHintLimited(sourceTimes, rankedHint(rank, firstSubmatch(answerHintSourceTimeRE, content)), answerHintMaxWeakValues)
		}
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
	if len(eventTimes) == 0 && len(observed) > 0 {
		parts = append(parts, "weak_observed_at="+strings.Join(observed, "; "))
	}
	if len(eventTimes) == 0 && len(sourceTimes) > 0 {
		parts = append(parts, "source_time_anchors="+strings.Join(sourceTimes, "; "))
	}
	parts = append(parts, "rule=prefer top-ranked [time:] and explicit relative wording; do not recompute a relative phrase on top of [time:]; ignore weak observed/source timestamps unless the memory text says the event happened at that turn")
	return "ANSWER_HINTS: " + strings.Join(parts, " | ")
}

func buildNumericAnswerHints(artifacts []runners.RecallArtifact) string {
	var candidates []string
	for rank, artifact := range artifacts {
		for _, raw := range answerHintNumericRE.FindAllString(artifact.Content, 6) {
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

func answerHintDateSearchText(text string) string {
	text = answerHintObservedAtRE.ReplaceAllString(text, " ")
	text = answerHintSourceTimeRE.ReplaceAllString(text, " ")
	return text
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
	return appendUniqueHintLimited(values, value, 0)
}

func appendUniqueHintLimited(values []string, value string, limit int) []string {
	if limit > 0 && len(values) >= limit {
		return values
	}
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
