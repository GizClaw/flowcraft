package words

import (
	"strings"

	"github.com/GizClaw/flowcraft/memory/text/normalize"
	"github.com/GizClaw/flowcraft/memory/text/phrase"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

var semanticTokenizer = tokenize.NewMultilingual()

var yesNoStartPhrases = [][]string{
	{"is"}, {"are"}, {"am"}, {"was"}, {"were"},
	{"did"}, {"does"}, {"do"},
	{"has"}, {"have"}, {"had"},
	{"can"}, {"could"}, {"will"}, {"would"},
	{"should"}, {"shall"}, {"may"}, {"might"}, {"must"},
}

var yesNoCuePhrases = [][]string{
	{"whether"},
	{"yes", "or", "no"},
	{"true", "or", "false"},
}

var negationCueTokens = []string{
	"not", "never", "no", "cannot", "without",
	"n't", "isn't", "wasn't", "aren't", "weren't", "can't", "won't", "don't", "doesn't", "didn't",
}

var cancellationCuePhrases = [][]string{
	{"cancel"}, {"canceled"}, {"cancelled"}, {"called", "off"}, {"no", "longer"},
}

var counterfactualCuePhrases = [][]string{
	{"would", "have"},
	{"could", "have"},
	{"should", "have"},
	{"if", "only"},
	{"otherwise"},
	{"instead", "of"},
	{"hypothetical"},
	{"counterfactual"},
}

var planCuePhrases = [][]string{
	{"plan", "to"}, {"plans", "to"}, {"planned", "to"},
	{"will"}, {"going", "to"}, {"scheduled", "to"},
}

var desiredCuePhrases = [][]string{
	{"want", "to"}, {"wants", "to"}, {"wanted", "to"},
	{"hope", "to"}, {"hopes", "to"}, {"would", "like"},
}

var suggestionCuePhrases = [][]string{
	{"suggested"}, {"recommend"}, {"recommended"}, {"recommendation"},
}

var hypotheticalCuePhrases = [][]string{
	{"could"}, {"might"}, {"may"}, {"possible"}, {"possibly"},
	{"hypothetical"}, {"suppose"}, {"assuming"},
}

var likelyCuePhrases = [][]string{
	{"probably"}, {"likely"}, {"seems"}, {"seem"}, {"appears"}, {"appear"},
}

var uncertainCuePhrases = [][]string{
	{"might"}, {"maybe"}, {"possibly"}, {"perhaps"}, {"unsure"}, {"uncertain"},
	{"not", "sure"}, {"unclear"},
}

var unknownCuePhrases = [][]string{
	{"unknown"}, {"not", "known"}, {"do", "not", "know"}, {"don't", "know"},
	{"unresolved"}, {"not", "resolved"},
}

// CanonicalSurface is the common lowercase, punctuation-insensitive text form
// for lightweight query/source cue matching.
func CanonicalSurface(text string) string {
	text = normalize.ReplaceNonAlnumWithSpace(text)
	text = normalize.CollapseSpaces(text)
	return strings.ToLower(text)
}

// SemanticQueryTerms returns deduplicated query tokens in the same multilingual
// vocabulary used by memory/text.
func SemanticQueryTerms(text string) []string {
	tokens := semanticTokenizer.Tokenize(text)
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if len([]rune(token)) < 2 {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
}

func HasYesNoVerificationCue(text string) bool {
	phrases := phrase.New(text)
	if containsAnyPhrase(phrases, yesNoCuePhrases) || phrases.ContainsAnyLiteral("吗", "是否") {
		return true
	}
	words := tokenize.SplitWords(text)
	if len(words) == 0 {
		return false
	}
	first := strings.ToLower(words[0])
	for _, prefix := range yesNoStartPhrases {
		if len(prefix) == 1 && first == prefix[0] {
			return true
		}
	}
	return false
}

func HasNegationCue(text string) bool {
	phrases := phrase.New(text)
	if phrases.ContainsAnyLiteral("不是", "没有", "从未", "未") {
		return true
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "n't") {
		return true
	}
	surface := " " + CanonicalSurface(text) + " "
	for _, cue := range negationCueTokens {
		if strings.Contains(surface, " "+CanonicalSurface(cue)+" ") {
			return true
		}
	}
	return false
}

func HasCancellationCue(text string) bool {
	phrases := phrase.New(text)
	return containsAnyPhrase(phrases, cancellationCuePhrases) ||
		phrases.ContainsAnyLiteral("取消", "不再")
}

func HasCounterfactualCue(text string) bool {
	phrases := phrase.New(text)
	return containsAnyPhrase(phrases, counterfactualCuePhrases) ||
		hasCounterfactualModalHave(text) ||
		phrases.ContainsAnyLiteral("本来", "否则")
}

func hasCounterfactualModalHave(text string) bool {
	words := tokenize.SplitWords(text)
	for i, word := range words {
		switch strings.ToLower(word) {
		case "would", "could", "should":
			for j := i + 1; j < len(words) && j <= i+3; j++ {
				if strings.EqualFold(words[j], "have") {
					return true
				}
			}
		}
	}
	return false
}

func HasPlanCue(text string) bool {
	return containsAnyPhrase(phrase.New(text), planCuePhrases)
}

func HasDesiredCue(text string) bool {
	return containsAnyPhrase(phrase.New(text), desiredCuePhrases)
}

func HasSuggestionCue(text string) bool {
	return containsAnyPhrase(phrase.New(text), suggestionCuePhrases)
}

func HasHypotheticalCue(text string) bool {
	phrases := phrase.New(text)
	return containsAnyPhrase(phrases, hypotheticalCuePhrases) ||
		phrases.ContainsAnyLiteral("假设", "可能")
}

func HasLikelyCue(text string) bool {
	return containsAnyPhrase(phrase.New(text), likelyCuePhrases)
}

func HasUncertainCue(text string) bool {
	phrases := phrase.New(text)
	return containsAnyPhrase(phrases, uncertainCuePhrases) ||
		phrases.ContainsAnyLiteral("不确定", "也许", "可能")
}

func HasUnknownCue(text string) bool {
	phrases := phrase.New(text)
	return containsAnyPhrase(phrases, unknownCuePhrases) ||
		phrases.ContainsAnyLiteral("未知", "不知道", "未确定")
}
