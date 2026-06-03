package words

import (
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/text/stopword"
)

var intentEntityStopwords = stopword.MultilingualOnlySet().Extend(
	"who", "what", "when", "where", "how", "which", "whom", "whose", "why",
	"the", "a", "an", "this", "that", "these", "those",
	"is", "are", "was", "were", "be", "been", "being", "am",
	"have", "has", "had", "having", "do", "does", "did", "done",
	"would", "could", "should", "might", "must", "shall",
	"of", "to", "in", "for", "on", "with", "at", "by", "from",
	"and", "or", "but", "if", "as", "about", "into", "than", "then",
	"i", "me", "my", "we", "our", "you", "your", "he", "she", "they",
	"it", "its", "his", "her", "their", "them", "him", "us",
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
	"i", "me", "mine", "myself", "you", "he", "she", "it", "we", "they",
	"i'm", "i’m", "i'll", "i’ll", "i've", "i’ve", "i'd", "i’d",
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

var entityAnchorFunctionTokens = stopword.MultilingualOnlySet().Extend(
	"of", "on", "in", "at", "by", "to", "from", "for", "with",
	"about", "into", "onto", "over", "under", "as",
)

var relativeTimeEntityTokens = stopword.NewSet().Extend(
	"today", "tomorrow", "yesterday", "next", "last", "ago",
	"hoy", "mañana", "manana", "ayer", "próximo", "proximo", "siguiente", "pasado", "hace",
	"aujourd'hui", "aujourdhui", "demain", "hier", "prochain", "dernier",
	"heute", "morgen", "gestern", "nächste", "naechste", "letzte", "vor",
	"hoje", "amanhã", "amanha", "ontem", "próximo", "proximo", "passado", "atrás", "atras",
	"vandaag", "morgen", "gisteren", "volgende", "vorige", "geleden",
	"сегодня", "завтра", "вчера", "следующий", "прошлый", "назад",
	"今天", "明天", "昨天", "下次", "上次", "之前",
)

// IsIntentEntityStopword is scoped to low-risk query entity extraction. Callers
// must not treat the remaining entity tokens as semantic slot evidence; verbs
// and function-like words can be valid query meaning even when they are poor
// entity anchors.
func IsIntentEntityStopword(token string) bool {
	return intentEntityStopwords.Contains(token)
}

func IsStructurizerEntityStopword(token string) bool {
	if strings.EqualFold(token, "will") {
		return false
	}
	return structurizerEntityStopwords.Contains(token)
}

// IsInvalidEntityAnchorToken reports whether a single token is structurally
// unsuitable as an entity anchor. It covers function words and date surfaces
// that should be represented by time metadata rather than entity fields.
func IsInvalidEntityAnchorToken(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return true
	}
	if IsStructurizerEntityStopword(token) || entityAnchorFunctionTokens.Contains(token) || relativeTimeEntityTokens.Contains(token) {
		return true
	}
	if len([]rune(token)) < 3 {
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
