package words

import (
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/text/stopword"
)

var intentEntityStopwords = stopword.MultilingualSet().Extend(
	"whom", "whose", "why", "done",
	"am", "having", "would", "could", "should", "might", "must",
	"again", "also", "just", "very", "too", "so", "yes",
	"mine", "yours", "hers", "ours", "theirs",
	"meet", "met", "meeting",
	"tell", "told", "say", "said", "know", "knew",
	"conocer", "conoció", "conocio", "decir", "dijo", "saber", "sabía", "sabia",
	"rencontrer", "rencontré", "rencontre", "dire", "dit", "savoir",
	"treffen", "traf", "sagen", "sagte", "wissen", "wusste",
	"conhecer", "conheceu", "dizer", "disse", "saber", "sabia",
	"ontmoeten", "ontmoette", "zeggen", "zei", "weten", "wist",
	"встретил", "встретила", "сказал", "сказала", "знать", "знал",
	"谁", "什么", "哪里", "哪儿", "什么时候", "多少", "几个", "知道", "说",
)

var structurizerEntityStopwords = stopword.MultilingualOnlySet().Extend(
	"i", "you", "he", "she", "it", "we", "they",
	"the", "a", "an", "this", "that", "these", "those",
	"my", "your", "his", "her", "its", "our", "their",
	"and", "or", "but", "so", "yes", "no", "ok", "okay",
	"yo", "tú", "tu", "él", "el", "ella", "nosotros", "ellos", "ellas", "sí", "si",
	"je", "tu", "il", "elle", "nous", "vous", "ils", "elles", "oui", "non",
	"ich", "du", "er", "sie", "wir", "ihr", "ja", "nein",
	"eu", "você", "voce", "ele", "ela", "nós", "nos", "eles", "elas", "sim", "não", "nao",
	"ik", "jij", "hij", "zij", "wij", "ja", "nee",
	"я", "ты", "он", "она", "мы", "они", "да", "нет",
	"我", "你", "他", "她", "它", "我们", "你们", "他们", "她们", "是", "不是", "好的",
)

var extractorEntityFunctionWords = stopword.MultilingualSet().Extend(
	"of", "on", "in", "at", "by", "to", "from", "for", "with",
	"about", "into", "onto", "over", "under", "as",
)

var extractorAbstractGerundEntityTokens = stopword.MultilingualSet().Extend(
	"being", "doing", "having", "making", "taking", "finding", "getting", "going", "using",
	"considering", "creating", "hoping", "looking", "planning", "seeing", "trying", "working", "writing",
)

var extractorWeakEntityFragmentTokens = stopword.MultilingualSet().Extend(
	"d", "ll", "m", "re", "s", "t", "ve",
)

var firstPersonSingularExtractorSubjectTokens = map[string]struct{}{
	"i":    {},
	"me":   {},
	"my":   {},
	"mine": {},
}

var extractorWeakEntityPhrasePrefixes = [][]string{
	{"considering", "adopting"},
	{"trying", "to"},
	{"hoping", "to"},
	{"planning", "to"},
	{"enough", "to"},
	{"able", "to"},
}

var relativeTimeEntityTokens = stopword.MultilingualSet().Extend(
	"today", "tomorrow", "yesterday", "next", "last", "ago",
)

func IsIntentEntityStopword(token string) bool {
	return intentEntityStopwords.Contains(token)
}

func IsStructurizerEntityStopword(token string) bool {
	if strings.EqualFold(token, "will") {
		return false
	}
	return structurizerEntityStopwords.Contains(token)
}

func IsExtractorEntityFunctionWord(token string) bool {
	return extractorEntityFunctionWords.Contains(token)
}

func IsExtractorAbstractGerundEntityToken(token string) bool {
	return extractorAbstractGerundEntityTokens.Contains(token)
}

func IsWeakExtractorEntityPhrase(tokens []string) bool {
	if len(tokens) == 0 {
		return true
	}
	if allWeakExtractorEntityPhraseTokens(tokens) {
		return true
	}
	for _, prefix := range extractorWeakEntityPhrasePrefixes {
		if hasTokenPrefix(tokens, prefix) {
			return true
		}
	}
	return IsExtractorAbstractGerundEntityToken(tokens[0])
}

func IsFirstPersonSingularExtractorSubject(tokens []string) bool {
	if len(tokens) == 0 {
		return false
	}
	if len(tokens) == 1 {
		_, ok := firstPersonSingularExtractorSubjectTokens[tokens[0]]
		return ok
	}
	return tokens[0] == "i" && allWeakExtractorEntityPhraseTokens(tokens[1:])
}

func allWeakExtractorEntityPhraseTokens(tokens []string) bool {
	for _, token := range tokens {
		if token == "" {
			continue
		}
		if IsStructurizerEntityStopword(token) ||
			IsExtractorEntityFunctionWord(token) ||
			extractorWeakEntityFragmentTokens.Contains(token) {
			continue
		}
		return false
	}
	return true
}

func hasTokenPrefix(tokens []string, prefix []string) bool {
	if len(tokens) < len(prefix) {
		return false
	}
	for i, token := range prefix {
		if tokens[i] != token {
			return false
		}
	}
	return true
}

func IsRelativeTimeEntityToken(token string) bool {
	return relativeTimeEntityTokens.Contains(token)
}

func IsCalendarEntityToken(token string) bool {
	if len([]rune(strings.TrimSpace(token))) < 3 {
		return false
	}
	for month := time.January; month <= time.December; month++ {
		if strings.EqualFold(token, month.String()) {
			return true
		}
	}
	for weekday := time.Sunday; weekday <= time.Saturday; weekday++ {
		if strings.EqualFold(token, weekday.String()) {
			return true
		}
	}
	return false
}
