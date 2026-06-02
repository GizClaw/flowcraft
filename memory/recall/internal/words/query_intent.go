package words

import (
	"slices"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/text/phrase"
)

var questionTokens = []string{
	"who", "what", "when", "where", "which", "how",
	"quien", "quién", "que", "qué", "cuando", "cuándo", "donde", "dónde", "cual", "cuál", "como", "cómo", "cuanto", "cuánto",
	"qui", "quoi", "quand", "ou", "où", "quel", "quelle", "comment", "combien",
	"wer", "was", "wann", "wo", "welcher", "welche", "welches", "wie",
	"quem", "que", "quando", "onde", "qual", "como", "quanto",
	"wie", "wat", "wanneer", "waar", "welke", "hoe", "hoeveel",
	"кто", "что", "когда", "где", "какой", "какая", "какое", "как", "сколько",
}

var questionLiterals = []string{
	"?", "？",
	"什么", "哪", "几", "多少", "多久", "多长时间", "吗",
}

var subjectInferenceLiterals = []string{
	"'s ",
}

var temporalIntentTokens = []string{
	"when", "date", "day", "month", "year", "time", "timeline", "chronology", "chronological",
	"earliest", "latest", "recent", "before", "after", "during", "since", "until",
	"cuándo", "cuando", "fecha", "día", "dia", "mes", "año", "ano", "tiempo", "antes", "después", "despues", "durante", "desde", "hasta",
	"quand", "date", "jour", "mois", "année", "annee", "temps", "avant", "après", "apres", "pendant", "depuis", "jusqu",
	"wann", "datum", "tag", "monat", "jahr", "zeit", "bevor", "nach", "während", "wahrend", "seit", "bis",
	"quando", "data", "dia", "mês", "mes", "ano", "tempo", "antes", "depois", "durante", "desde", "até", "ate",
	"wanneer", "datum", "dag", "maand", "jaar", "tijd", "voor", "na", "tijdens", "sinds", "tot",
	"когда", "дата", "день", "месяц", "год", "время", "раньше", "позже", "до", "после", "во", "с", "самый",
}

var temporalIntentPhrases = [][]string{
	{"what", "date"},
	{"which", "date"},
	{"what", "day"},
	{"which", "day"},
	{"what", "month"},
	{"which", "month"},
	{"what", "year"},
	{"which", "year"},
	{"at", "what", "time"},
	{"by", "when"},
	{"since", "when"},
	{"until", "when"},
	{"how", "long"},
	{"how", "old"},
	{"chronological", "order"},
	{"qué", "fecha"},
	{"que", "fecha"},
	{"qué", "día"},
	{"que", "dia"},
	{"qué", "año"},
	{"que", "ano"},
	{"desde", "cuándo"},
	{"desde", "cuando"},
	{"hasta", "cuándo"},
	{"hasta", "cuando"},
	{"cuánto", "tiempo"},
	{"cuanto", "tiempo"},
	{"quelle", "date"},
	{"quel", "jour"},
	{"quelle", "année"},
	{"quelle", "annee"},
	{"depuis", "quand"},
	{"jusqu", "à", "quand"},
	{"combien", "de", "temps"},
	{"welches", "datum"},
	{"welcher", "tag"},
	{"welches", "jahr"},
	{"seit", "wann"},
	{"bis", "wann"},
	{"wie", "lange"},
	{"que", "data"},
	{"que", "dia"},
	{"que", "ano"},
	{"desde", "quando"},
	{"até", "quando"},
	{"ate", "quando"},
	{"quanto", "tempo"},
	{"welke", "datum"},
	{"welke", "dag"},
	{"welk", "jaar"},
	{"sinds", "wanneer"},
	{"tot", "wanneer"},
	{"hoe", "lang"},
	{"какая", "дата"},
	{"какой", "день"},
	{"какой", "год"},
	{"с", "какого", "времени"},
	{"до", "какого", "времени"},
	{"как", "долго"},
}

var temporalIntentLiterals = []string{
	"什么时候", "哪天", "哪一天", "哪年", "哪一年", "几号", "几月", "多久", "多长时间",
	"最早", "最晚", "之前", "之后", "期间", "时间线",
}

var durationIntentPhrases = [][]string{
	{"how", "long"},
	{"how", "many", "days"},
	{"how", "many", "weeks"},
	{"how", "many", "months"},
	{"how", "many", "years"},
	{"how", "old"},
	{"since", "when"},
	{"for", "how", "long"},
	{"cuánto", "tiempo"},
	{"cuanto", "tiempo"},
	{"cuántos", "días"},
	{"cuantos", "dias"},
	{"cuántos", "meses"},
	{"cuantos", "meses"},
	{"cuántos", "años"},
	{"cuantos", "anos"},
	{"combien", "de", "temps"},
	{"combien", "de", "jours"},
	{"combien", "de", "mois"},
	{"combien", "d", "années"},
	{"combien", "d", "annees"},
	{"wie", "lange"},
	{"wie", "viele", "tage"},
	{"wie", "viele", "monate"},
	{"wie", "viele", "jahre"},
	{"quanto", "tempo"},
	{"quantos", "dias"},
	{"quantos", "meses"},
	{"quantos", "anos"},
	{"hoe", "lang"},
	{"hoeveel", "dagen"},
	{"hoeveel", "maanden"},
	{"hoeveel", "jaren"},
	{"как", "долго"},
	{"сколько", "дней"},
	{"сколько", "месяцев"},
	{"сколько", "лет"},
}

var durationIntentLiterals = []string{
	"多久", "多长时间", "几天", "几周", "几个月", "几年", "多大",
}

var numericIntentTokens = []string{
	"number", "count", "total", "amount", "quantity", "age", "frequency", "percent", "percentage", "rank", "order", "ordinal", "score", "price", "cost",
	"número", "numero", "cuenta", "total", "cantidad", "edad", "frecuencia", "porcentaje", "precio", "costo",
	"nombre", "total", "montant", "quantité", "quantite", "âge", "age", "fréquence", "frequence", "pourcentage", "prix", "coût", "cout",
	"anzahl", "nummer", "gesamt", "betrag", "menge", "alter", "häufigkeit", "haufigkeit", "prozent", "preis", "kosten",
	"número", "numero", "total", "quantidade", "idade", "frequência", "frequencia", "porcentagem", "percentual", "preço", "preco", "custo",
	"nummer", "aantal", "totaal", "hoeveelheid", "leeftijd", "frequentie", "percentage", "prijs", "kosten",
	"номер", "количество", "сумма", "возраст", "частота", "процент", "цена", "стоимость",
}

var numericIntentPhrases = [][]string{
	{"how", "many"},
	{"how", "much"},
	{"how", "often"},
	{"how", "old"},
	{"how", "long"},
	{"what", "number"},
	{"which", "number"},
	{"how", "many", "times"},
	{"cuántos"},
	{"cuantas"},
	{"cuántas"},
	{"cuantos"},
	{"cuánto"},
	{"cuanto"},
	{"qué", "número"},
	{"que", "numero"},
	{"cuántas", "veces"},
	{"cuantas", "veces"},
	{"con", "qué", "frecuencia"},
	{"con", "que", "frecuencia"},
	{"combien"},
	{"quel", "nombre"},
	{"quel", "numéro"},
	{"quel", "numero"},
	{"combien", "de", "fois"},
	{"à", "quelle", "fréquence"},
	{"a", "quelle", "frequence"},
	{"wie", "viele"},
	{"wie", "viel"},
	{"welche", "nummer"},
	{"wie", "oft"},
	{"quantos"},
	{"quantas"},
	{"quanto"},
	{"quanta"},
	{"que", "número"},
	{"que", "numero"},
	{"quantas", "vezes"},
	{"com", "que", "frequência"},
	{"com", "que", "frequencia"},
	{"hoeveel"},
	{"hoe", "vaak"},
	{"welk", "nummer"},
	{"welke", "nummer"},
	{"сколько"},
	{"как", "часто"},
	{"какой", "номер"},
	{"сколько", "раз"},
}

var numericIntentLiterals = []string{
	"多少", "几个", "几次", "多少次", "多大", "第几", "多频繁", "频率", "数量", "总数", "年龄", "百分比", "金额", "价格",
}

// HasTemporalQuestionCue reports whether text asks for temporal information.
func HasTemporalQuestionCue(text string) bool {
	phrases := phrase.New(text)
	if phrases.ContainsAnyLiteral(temporalIntentLiterals...) ||
		containsAnyPhrase(phrases, temporalIntentPhrases) ||
		phrases.ContainsAny("when", "cuándo", "cuando", "quand", "wann", "quando") {
		return true
	}
	if isQuestionLike(text) && containsAnyToken(phrases, temporalIntentTokens) {
		return true
	}
	return false
}

// TemporalIntentKinds returns finer-grained temporal question categories.
func TemporalIntentKinds(text string) []domain.QueryTemporalIntentKind {
	phrases := phrase.New(text)
	var out []domain.QueryTemporalIntentKind
	add := func(kind domain.QueryTemporalIntentKind) {
		if !hasTemporalKind(out, kind) {
			out = append(out, kind)
		}
	}
	if HasDurationQuestionCue(text) {
		add(domain.QueryTemporalIntentDuration)
	}
	if phrases.ContainsAnyLiteral("最早", "最晚", "时间线") ||
		phrases.ContainsAny("earliest", "latest", "chronology", "chronological", "recent") {
		add(domain.QueryTemporalIntentOrder)
	}
	if phrases.ContainsAnyLiteral("之前", "之后", "期间") ||
		phrases.ContainsAny("before", "after", "during", "since", "until",
			"antes", "después", "despues", "durante", "desde", "hasta",
			"avant", "après", "apres", "pendant", "depuis",
			"bevor", "nach", "während", "wahrend", "seit", "bis",
			"voor", "na", "tijdens", "sinds", "tot",
			"до", "после") {
		add(domain.QueryTemporalIntentRange)
	}
	if HasTemporalQuestionCue(text) {
		add(domain.QueryTemporalIntentDate)
	}
	return out
}

// HasDurationQuestionCue reports whether text asks for elapsed time, age, or
// duration-like quantities.
func HasDurationQuestionCue(text string) bool {
	phrases := phrase.New(text)
	return phrases.ContainsAnyLiteral(durationIntentLiterals...) ||
		containsAnyPhrase(phrases, durationIntentPhrases)
}

// NumericIntentKinds returns finer-grained numeric question categories.
func NumericIntentKinds(text string) []domain.QueryNumericIntentKind {
	phrases := phrase.New(text)
	var out []domain.QueryNumericIntentKind
	add := func(kind domain.QueryNumericIntentKind) {
		if !hasNumericKind(out, kind) {
			out = append(out, kind)
		}
	}
	if HasDurationQuestionCue(text) {
		add(domain.QueryNumericIntentDuration)
	}
	if phrases.ContainsAnyLiteral("年龄", "多大") ||
		phrases.ContainsAny("age", "old", "edad", "âge", "alter", "idade", "leeftijd", "возраст") ||
		phrases.ContainsPhrase("how", "old") {
		add(domain.QueryNumericIntentAge)
	}
	if phrases.ContainsAnyLiteral("几次", "多少次", "多频繁", "频率") ||
		phrases.ContainsAny("frequency", "frecuencia", "fréquence", "frequence", "häufigkeit", "haufigkeit", "frequência", "frequencia", "frequentie", "частота") ||
		phrases.ContainsPhrase("how", "often") || phrases.ContainsPhrase("how", "many", "times") ||
		phrases.ContainsPhrase("cuántas", "veces") || phrases.ContainsPhrase("cuantas", "veces") ||
		phrases.ContainsPhrase("combien", "de", "fois") || phrases.ContainsPhrase("wie", "oft") ||
		phrases.ContainsPhrase("quantas", "vezes") || phrases.ContainsPhrase("hoe", "vaak") ||
		phrases.ContainsPhrase("как", "часто") || phrases.ContainsPhrase("сколько", "раз") {
		add(domain.QueryNumericIntentFrequency)
	}
	if phrases.ContainsAnyLiteral("第几") ||
		phrases.ContainsAny("rank", "order", "ordinal", "номер") {
		add(domain.QueryNumericIntentOrdinal)
	}
	if phrases.ContainsAnyLiteral("金额", "价格") ||
		phrases.ContainsAny("price", "cost", "precio", "costo", "prix", "coût", "cout", "preis", "kosten", "preço", "preco", "prijs", "цена", "стоимость") {
		add(domain.QueryNumericIntentPrice)
	}
	if phrases.ContainsAnyLiteral("百分比") ||
		phrases.ContainsAny("percent", "percentage", "porcentaje", "pourcentage", "prozent", "porcentagem", "percentual", "процент") {
		add(domain.QueryNumericIntentPercent)
	}
	if phrases.ContainsAnyLiteral("几个", "数量", "总数") ||
		phrases.ContainsAny("number", "count", "total", "número", "numero", "nombre", "anzahl", "aantal", "количество") ||
		phrases.ContainsPhrase("how", "many") || phrases.ContainsPhrase("wie", "viele") ||
		phrases.ContainsPhrase("hoeveel") || phrases.ContainsPhrase("сколько") {
		add(domain.QueryNumericIntentCount)
	}
	if phrases.ContainsAnyLiteral("多少", "金额") ||
		phrases.ContainsAny("amount", "quantity", "montant", "quantité", "quantite", "betrag", "menge", "quantidade", "hoeveelheid", "сумма") ||
		phrases.ContainsPhrase("how", "much") || phrases.ContainsPhrase("wie", "viel") {
		add(domain.QueryNumericIntentAmount)
	}
	if len(out) == 0 && (phrases.ContainsAnyLiteral(numericIntentLiterals...) ||
		containsAnyPhrase(phrases, numericIntentPhrases) ||
		(isQuestionLike(text) && containsAnyToken(phrases, numericIntentTokens))) {
		add(domain.QueryNumericIntentAmount)
	}
	return out
}

func isQuestionLike(text string) bool {
	phrases := phrase.New(text)
	return phrases.ContainsAnyLiteral(questionLiterals...) ||
		containsAnyToken(phrases, questionTokens)
}

// HasSubjectInferenceCue reports whether the surface explicitly marks a
// possessive subject. Plain questions do not infer a structured subject because
// that would activate relation/profile sources from a weak first-entity guess.
func HasSubjectInferenceCue(text string) bool {
	return phrase.New(text).ContainsAnyLiteral(subjectInferenceLiterals...)
}

func containsAnyToken(phrases phrase.Matcher, tokens []string) bool {
	return slices.ContainsFunc(tokens, phrases.Contains)
}

func containsAnyPhrase(phrases phrase.Matcher, phraseList [][]string) bool {
	for _, candidate := range phraseList {
		if phrases.ContainsPhrase(candidate...) {
			return true
		}
	}
	return false
}

func hasTemporalKind(kinds []domain.QueryTemporalIntentKind, want domain.QueryTemporalIntentKind) bool {
	return slices.Contains(kinds, want)
}

func hasNumericKind(kinds []domain.QueryNumericIntentKind, want domain.QueryNumericIntentKind) bool {
	return slices.Contains(kinds, want)
}
