package words

import (
	"strings"

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

func IsIntentEntityStopword(token string) bool {
	return intentEntityStopwords.Contains(token)
}

func IsStructurizerEntityStopword(token string) bool {
	if strings.EqualFold(token, "will") {
		return false
	}
	return structurizerEntityStopwords.Contains(token)
}
