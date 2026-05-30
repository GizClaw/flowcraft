package words

import (
	"slices"
	"strings"
	"unicode"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/text/phrase"
	"github.com/GizClaw/flowcraft/memory/text/stopword"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

var querySurfaceStopwords = stopword.MultilingualSet().Extend(
	"am", "hers", "ours", "theirs",
)

var temporalQuestionWords = map[string]struct{}{
	"when": {}, "date": {}, "time": {}, "did": {}, "does": {}, "was": {}, "were": {}, "is": {}, "are": {},
	"cuando": {}, "cuándo": {}, "fecha": {}, "tiempo": {}, "fue": {}, "eran": {},
	"quand": {}, "temps": {}, "était": {}, "etait": {},
	"wann": {}, "datum": {}, "zeit": {}, "war": {}, "waren": {},
	"quando": {}, "data": {}, "tempo": {}, "foi": {},
	"wanneer": {}, "tijd": {},
	"когда": {}, "дата": {}, "время": {}, "был": {}, "была": {}, "были": {},
}

var bridgeConnectors = []string{
	" that ", " which ", " who ", " because ", " before ", " after ",
	" que ", " quien ", " quién ", " porque ", " antes ", " después ", " despues ",
	" qui ", " parce que ", " avant ", " après ", " apres ",
	" welcher ", " welche ", " welches ", " weil ", " bevor ", " nachdem ",
	" quem ", " antes de ", " depois de ",
	" dat ", " omdat ",
	" который ", " которая ", " которое ", " кто ", " потому что ", " до ", " после ",
	"因为", "之前", "之后",
}

var collectionCueTokens = []string{
	"item", "thing", "event", "activ", "activity", "kind", "type", "style",
	"name", "person", "people", "place", "medium", "media", "artist",
	"band", "book", "movie", "song", "sport", "country", "food", "breed", "pet",
	"artículo", "articulo", "cosa", "evento", "actividad", "tipo", "nombre", "persona", "lugar", "libro", "película", "pelicula", "canción", "cancion", "deporte", "país", "pais", "comida", "mascota",
	"élément", "element", "chose", "événement", "evenement", "activité", "activite", "genre", "type", "nom", "personne", "lieu", "livre", "film", "chanson", "sport", "pays", "nourriture", "animal",
	"ding", "ereignis", "aktivität", "aktivitat", "art", "typ", "name", "person", "ort", "buch", "film", "lied", "sport", "land", "essen", "haustier",
	"coisa", "evento", "atividade", "tipo", "nome", "pessoa", "lugar", "livro", "filme", "música", "musica", "esporte", "país", "pais", "comida", "animal",
	"voorwerp", "ding", "gebeurtenis", "activiteit", "soort", "type", "naam", "persoon", "plek", "plaats", "boek", "film", "lied", "sport", "land", "eten", "huisdier",
	"вещь", "событие", "активность", "тип", "имя", "человек", "место", "книга", "фильм", "песня", "спорт", "страна", "еда", "питомец",
}

var collectionQuestionTokens = []string{
	"what", "which",
	"qué", "que", "cuál", "cual",
	"quel", "quelle",
	"welcher", "welche", "welches",
	"qual", "quais",
	"welk", "welke",
	"какой", "какая", "какое", "какие",
}

var collectionCountPhrases = [][]string{
	{"how", "many"},
	{"how", "much"},
	{"cuántos"},
	{"cuantos"},
	{"cuántas"},
	{"cuantas"},
	{"cuánto"},
	{"cuanto"},
	{"combien"},
	{"wie", "viele"},
	{"wie", "viel"},
	{"quantos"},
	{"quantas"},
	{"quanto"},
	{"quanta"},
	{"hoeveel"},
	{"сколько"},
}

var collectionPossessionTokens = []string{
	"has", "have",
	"tiene", "tienen",
	"a", "ont",
	"hat", "haben",
	"tem", "têm", "tens",
	"heeft", "hebben",
	"есть", "имеет", "имеют",
}

var collectionQuestionLiterals = []string{
	"哪些", "哪种", "几", "几个", "多少",
}

var collectionCueLiterals = []string{
	"物品", "东西", "事件", "活动", "类型", "名字", "人", "地点", "书", "电影", "歌曲", "运动", "国家", "食物", "宠物",
}

var bridgeSurfacePhrases = [][]string{
	{"that"},
	{"which"},
	{"who"},
	{"after"},
	{"before"},
	{"because"},
	{"que"},
	{"quien"},
	{"quién"},
	{"porque"},
	{"antes"},
	{"después"},
	{"despues"},
	{"qui"},
	{"qu"},
	{"avant"},
	{"après"},
	{"apres"},
	{"parce", "que"},
	{"welcher"},
	{"welche"},
	{"welches"},
	{"weil"},
	{"bevor"},
	{"nachdem"},
	{"quem"},
	{"depois"},
	{"dat"},
	{"omdat"},
	{"который"},
	{"которая"},
	{"которое"},
	{"кто"},
	{"до"},
	{"после"},
	{"потому", "что"},
}

var bridgePronounTokens = map[string]struct{}{
	"her":   {},
	"his":   {},
	"their": {},
	"su":    {},
	"sus":   {},
	"son":   {},
	"sa":    {},
	"ses":   {},
	"sein":  {},
	"seine": {},
	"ihr":   {},
	"ihre":  {},
	"seu":   {},
	"sua":   {},
	"zijn":  {},
	"haar":  {},
	"его":   {},
	"её":    {},
	"ее":    {},
	"их":    {},
}

var bridgeSurfaceLiterals = []string{
	"因为", "之前", "之后", "谁", "哪个", "哪一个", "他的", "她的", "他们的", "她们的",
}

var disambiguationSurfacePhrases = [][]string{
	{"instead"},
	{"rather", "than"},
	{"which", "one"},
}

var disambiguationSurfaceLiterals = []string{
	"还是", "或者", "而不是", "哪一个",
}

// SplitQueryWords returns raw surface words for query rewriting heuristics.
func SplitQueryWords(text string) []string {
	return tokenize.SplitWords(text)
}

// IsQueryStopword reports whether word is semantically weak for query rewrites.
func IsQueryStopword(word string) bool {
	word = strings.TrimSpace(word)
	return len([]rune(word)) <= 1 || querySurfaceStopwords.Contains(word)
}

// SignificantQueryTerms drops weak question/function words while preserving
// original casing for source-specific query variants.
func SignificantQueryTerms(text string) []string {
	words := SplitQueryWords(text)
	out := make([]string, 0, len(words))
	for _, word := range words {
		if IsQueryStopword(word) {
			continue
		}
		out = append(out, word)
	}
	return out
}

// SignificantQueryText returns a compact query variant or the original text
// when every term is filtered.
func SignificantQueryText(text string) string {
	terms := SignificantQueryTerms(text)
	if len(terms) == 0 {
		return text
	}
	return strings.Join(terms, " ")
}

// BridgeClauses splits common bridge-question clauses while preserving surface
// casing in each side of the split.
func BridgeClauses(text string) []string {
	lower := strings.ToLower(text)
	var out []string
	for _, connector := range bridgeConnectors {
		idx := strings.Index(lower, connector)
		if idx < 0 {
			continue
		}
		out = append(out, text[:idx], text[idx+len(connector):])
	}
	return out
}

// StripTemporalQuestionWords removes generic temporal question words for
// source-expansion variants.
func StripTemporalQuestionWords(text string) string {
	words := SplitQueryWords(text)
	out := make([]string, 0, len(words))
	for _, word := range words {
		if _, ok := temporalQuestionWords[strings.ToLower(word)]; ok {
			continue
		}
		out = append(out, word)
	}
	return strings.Join(out, " ")
}

// CollectionAnchorWords returns title-cased non-stopword anchors useful for
// collection/set completion rewrites.
func CollectionAnchorWords(text string) []string {
	words := SplitQueryWords(text)
	var out []string
	for _, word := range words {
		if IsQueryStopword(word) {
			continue
		}
		if unicode.IsUpper(firstRune(word)) {
			out = append(out, word)
		}
	}
	return out
}

// HasCollectionSurfaceCue reports whether query shape asks for a set/list.
func HasCollectionSurfaceCue(text string, tokens map[string]struct{}, numericKinds []domain.QueryNumericIntentKind) bool {
	if slicesContainsNumericIntent(numericKinds, domain.QueryNumericIntentCount) ||
		slicesContainsNumericIntent(numericKinds, domain.QueryNumericIntentFrequency) {
		return true
	}
	phrases := phrase.New(text)
	if containsSurfacePhrase(phrases, collectionCountPhrases) ||
		phrases.ContainsAnyLiteral(collectionQuestionLiterals...) {
		return true
	}
	if phrases.ContainsAny(collectionQuestionTokens...) {
		if HasAnyToken(tokens, collectionCueTokens...) || phrases.ContainsAnyLiteral(collectionCueLiterals...) {
			return true
		}
		return phrases.ContainsAny(collectionPossessionTokens...)
	}
	if phrases.ContainsAnyLiteral("什么") {
		return phrases.ContainsAnyLiteral(collectionCueLiterals...)
	}
	return false
}

// HasBridgeSurfaceCue reports whether query shape likely requires bridge facts.
func HasBridgeSurfaceCue(text string, proper map[string]struct{}) bool {
	if len(proper) >= 2 {
		return true
	}
	phrases := phrase.New(text)
	if phrases.ContainsAnyLiteral(bridgeSurfaceLiterals...) {
		return true
	}
	for _, cue := range bridgeSurfacePhrases {
		if phrases.ContainsPhrase(cue...) {
			return true
		}
	}
	for _, word := range SplitQueryWords(text) {
		if _, ok := bridgePronounTokens[strings.ToLower(word)]; ok {
			return true
		}
	}
	return false
}

// HasDisambiguationSurfaceCue reports whether query shape asks the planner to
// distinguish between alternatives.
func HasDisambiguationSurfaceCue(text string) bool {
	phrases := phrase.New(text)
	return phrases.ContainsAny("or") ||
		phrases.ContainsAnyLiteral(disambiguationSurfaceLiterals...) ||
		containsSurfacePhrase(phrases, disambiguationSurfacePhrases)
}

// HasAnyToken reports whether tokens contains any value.
func HasAnyToken(tokens map[string]struct{}, values ...string) bool {
	for _, value := range values {
		if _, ok := tokens[value]; ok {
			return true
		}
	}
	return false
}

func containsSurfacePhrase(phrases phrase.Matcher, phraseList [][]string) bool {
	for _, candidate := range phraseList {
		if phrases.ContainsPhrase(candidate...) {
			return true
		}
	}
	return false
}

func slicesContainsNumericIntent(kinds []domain.QueryNumericIntentKind, want domain.QueryNumericIntentKind) bool {
	return slices.Contains(kinds, want)
}

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}
