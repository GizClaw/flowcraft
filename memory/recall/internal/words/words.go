package words

import (
	"strings"

	"github.com/GizClaw/flowcraft/memory/text/stopword"
)

var temporalQuestionCues = []string{
	"when", "what date", "which day", "how long", "how old",
	"earliest", "latest", "before", "after", "during",
	"什么时候", "哪天", "多久", "多长时间", "最早", "最晚",
}

var intentTemporalQuestionCues = []string{
	"when", "what date", "which date", "how long",
	"how many days", "how many months", "how many years",
}

var numericIntentCues = []string{
	"how many", "how much", "number", "count", "total",
	"多少", "几个", "几次",
}

var intentEntityStopwords = stopword.EnglishSet().Extend(
	"whom", "whose", "why", "done",
	"am", "having", "would", "could", "should", "might", "must",
	"again", "also", "just", "very", "too", "so", "yes",
	"mine", "yours", "hers", "ours", "theirs",
	"meet", "met", "meeting",
	"tell", "told", "say", "said", "know", "knew",
)

var structurizerEntityStopwords = stopword.NewSet().Extend(
	"i", "you", "he", "she", "it", "we", "they",
	"the", "a", "an", "this", "that", "these", "those",
	"my", "your", "his", "her", "its", "our", "their",
	"and", "or", "but", "so", "yes", "no", "ok", "okay",
)

var proceduralVerbs = []string{
	"use ", "check ", "run ", "call ", "format ", "respond ",
	"return ", "ask ", "extract ", "parse ",
}

var proceduralPreferenceTargets = []string{
	"markdown", "table", "format", "output", "response", "answer",
}

func HasTemporalQuestionCue(text string) bool {
	return containsAny(strings.ToLower(text), temporalQuestionCues)
}

func HasIntentTemporalQuestionCue(text string) bool {
	return containsAny(strings.ToLower(text), intentTemporalQuestionCues)
}

func HasNumericIntentCue(text string) bool {
	return containsAny(strings.ToLower(text), numericIntentCues)
}

func IsIntentEntityStopword(token string) bool {
	return intentEntityStopwords.Contains(token)
}

func IsStructurizerEntityStopword(token string) bool {
	return structurizerEntityStopwords.Contains(token)
}

func LooksProcedural(content string) bool {
	s := strings.ToLower(strings.Join(strings.Fields(content), " "))
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "when ") && strings.Contains(s, ", ") {
		return true
	}
	if strings.HasPrefix(s, "before ") && strings.Contains(s, ", ") {
		return true
	}
	if (strings.HasPrefix(s, "first ") || strings.Contains(s, " first ")) && strings.Contains(s, " then ") {
		return true
	}
	if strings.Contains(s, "always ") {
		for _, verb := range proceduralVerbs {
			if strings.Contains(s, "always "+verb) {
				return true
			}
		}
	}
	if strings.Contains(s, "prefer") {
		for _, token := range proceduralPreferenceTargets {
			if strings.Contains(s, token) {
				return true
			}
		}
	}
	return false
}

func containsAny(text string, cues []string) bool {
	for _, cue := range cues {
		if strings.Contains(text, cue) {
			return true
		}
	}
	return false
}
