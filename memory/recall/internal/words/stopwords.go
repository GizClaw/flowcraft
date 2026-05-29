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

var extractorEntityFunctionWords = stopword.MultilingualOnlySet().Extend(
	"of", "on", "in", "at", "by", "to", "from", "for", "with",
	"about", "into", "onto", "over", "under", "as",
)

var extractorAbstractGerundEntityTokens = stopword.NewSet().Extend(
	"being", "doing", "having", "making", "taking", "finding", "getting", "going", "using",
	"considering", "creating", "hoping", "looking", "planning", "seeing", "trying", "working", "writing",
	"ser", "estar", "hacer", "tener", "tomar", "encontrar", "ir", "usar",
	"considerar", "crear", "esperar", "buscar", "planear", "planificar", "ver", "intentar", "trabajar", "escribir",
	"siendo", "haciendo", "teniendo", "tomando", "encontrando", "yendo", "usando",
	"considerando", "creando", "esperando", "buscando", "planeando", "planificando", "viendo", "intentando", "trabajando", "escribiendo",
	"être", "etre", "faire", "avoir", "aller", "utiliser", "considérer", "considerer",
	"créer", "creer", "espérer", "esperer", "chercher", "planifier", "voir", "essayer", "travailler", "écrire", "ecrire",
	"étant", "etant", "faisant", "ayant", "allant", "utilisant", "considérant", "considerant",
	"sein", "machen", "haben", "nehmen", "finden", "gehen", "benutzen", "verwenden",
	"überlegen", "ueberlegen", "erstellen", "hoffen", "suchen", "planen", "sehen", "versuchen", "arbeiten", "schreiben",
	"fazer", "ter", "usar", "considerar", "criar", "procurar", "planejar", "tentar",
	"sendo", "fazendo", "tendo", "indo", "criando", "procurando", "planejando", "tentando",
	"zijn", "doen", "hebben", "maken", "nemen", "vinden", "gaan", "gebruiken",
	"overwegen", "creëren", "creeren", "hopen", "zoeken", "plannen", "zien", "proberen", "werken", "schrijven",
	"быть", "делать", "иметь", "брать", "находить", "идти", "использовать", "рассматривать",
	"создавать", "надеяться", "искать", "планировать", "видеть", "пытаться", "работать", "писать",
	"做", "进行", "拥有", "制作", "寻找", "去", "使用", "考虑", "创建", "希望", "计划", "看", "尝试", "工作", "写",
)

var extractorWeakEntityFragmentTokens = stopword.NewSet().Extend(
	"d", "ll", "m", "re", "s", "t", "ve",
	"c", "j", "l", "n", "qu",
)

var firstPersonSingularExtractorSubjectTokens = stopword.NewSet().Extend(
	"i", "me", "my", "mine",
	"yo", "mí", "mi", "mío", "mia", "mía",
	"je", "moi", "mon", "ma", "mes",
	"ich", "mich", "mir", "mein", "meine",
	"eu", "meu", "minha",
	"ik", "mij", "mijn",
	"я", "меня", "мне", "мой", "моя",
	"我", "我的",
)

var extractorWeakEntityPhrasePrefixes = [][]string{
	{"considering", "adopting"},
	{"trying", "to"},
	{"hoping", "to"},
	{"planning", "to"},
	{"enough", "to"},
	{"able", "to"},
	{"considerando", "adoptar"},
	{"intentando"},
	{"tratando", "de"},
	{"esperando"},
	{"planeando"},
	{"planificando"},
	{"suficiente", "para"},
	{"capaz", "de"},
	{"essayant", "de"},
	{"espérant", "de"},
	{"esperant", "de"},
	{"planifiant"},
	{"assez", "pour"},
	{"capable", "de"},
	{"versuchen", "zu"},
	{"hoffend", "auf"},
	{"planen", "zu"},
	{"genug", "um"},
	{"fähig", "zu"},
	{"faehig", "zu"},
	{"tentando"},
	{"esperando"},
	{"planejando"},
	{"proberen", "te"},
	{"hopend", "op"},
	{"plannen", "om"},
	{"genoeg", "om"},
	{"пытаясь"},
	{"надеясь"},
	{"планируя"},
	{"尝试"},
	{"希望"},
	{"计划"},
	{"足够"},
	{"能够"},
}

var extractorWeakRelationObjectStartTokens = stopword.NewSet().Extend(
	"to", "being", "taking", "making", "going", "trying", "planning", "considering", "helping",
	"ser", "estar", "tomar", "ir", "intentar", "planear", "planificar", "considerar", "ayudar",
	"siendo", "tomando", "yendo", "intentando", "planeando", "planificando", "considerando", "ayudando",
	"être", "etre", "étant", "etant", "aller", "allant", "essayer", "planifier", "considérer", "considerer", "aider",
	"sein", "gehen", "versuchen", "planen", "überlegen", "ueberlegen", "helfen",
	"ir", "tentar", "planejar", "ajudar", "indo", "tentando", "planejando", "ajudando",
	"zijn", "gaan", "proberen", "plannen", "overwegen", "helpen",
	"быть", "идти", "пытаться", "планировать", "рассматривать", "помогать",
	"做", "进行", "去", "尝试", "计划", "考虑", "帮助",
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

var lowValueExtractorNotePrefixes = []string{
	"thanks", "thank you", "wow", "awesome", "nice", "cool", "glad", "congrats", "congratulations",
	"love that", "that sounds", "sounds like", "it sounds", "it's great", "it is great", "good to see",
	"happy for", "they must have felt", "i bet", "you are so", "you're so",
	"gracias", "felicidades", "suena", "me alegra",
	"merci", "felicitations", "félicitations", "ça sonne", "ca sonne", "je suis content",
	"danke", "glückwunsch", "glueckwunsch", "klingt",
	"obrigado", "obrigada", "parabéns", "parabens", "parece",
	"bedankt", "gefeliciteerd", "klinkt",
	"спасибо", "поздравляю", "звучит",
	"谢谢", "恭喜", "听起来",
}

var lowValueExtractorNoteFragments = []string{
	" sounds awesome", " sounds great",
	" suena genial", " suena muy bien",
	" sounds nice",
	" klingt toll", " klingt gut",
	" звучит здорово", " звучит хорошо",
	"听起来很棒", "听起来不错",
}

var safeFirstPersonExtractorContentVerbs = stopword.NewSet().Extend(
	"applied", "attended", "bought", "built", "came", "created", "crafted", "designed",
	"did", "drew", "fed", "felt", "finished", "found", "gave", "got", "had", "joined",
	"learned", "learnt", "lost", "made", "met", "moved", "painted", "played", "read",
	"received", "saw", "shared", "signed", "started", "submitted", "talked", "took",
	"tried", "used", "visited", "volunteered", "went", "won", "wrote",
	"can", "could", "would", "should", "must",
)

var thirdPersonPresentExtractorContentVerbs = map[string]string{
	"enjoy":    "enjoys",
	"feel":     "feels",
	"help":     "helps",
	"hope":     "hopes",
	"like":     "likes",
	"live":     "lives",
	"love":     "loves",
	"make":     "makes",
	"mentor":   "mentors",
	"need":     "needs",
	"plan":     "plans",
	"play":     "plays",
	"prefer":   "prefers",
	"remember": "remembers",
	"think":    "thinks",
	"use":      "uses",
	"want":     "wants",
	"work":     "works",
}

var unsupportedFirstPersonExtractorContentStarts = stopword.NewSet().Extend(
	"i", "my", "we", "our",
)

var abstractMadeRelationObjectTokens = stopword.NewSet().Extend(
	"call", "change", "connection", "connections", "decision", "difference",
	"donation", "donations", "energy", "friend", "friends", "happiness",
	"happy", "impact", "medal", "memories", "memory", "plan", "plans",
	"presentation", "progress", "purpose", "support", "view", "views",
)

var abstractMadeRelationObjectPhrases = [][]string{
	{"check", "up"},
	{"community", "work"},
	{"own", "family"},
	{"focused", "business"},
	{"efficient", "business"},
	{"toy", "drive"},
}

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
		return firstPersonSingularExtractorSubjectTokens.Contains(tokens[0])
	}
	return firstPersonSingularExtractorSubjectTokens.Contains(tokens[0]) && allWeakExtractorEntityPhraseTokens(tokens[1:])
}

func IsWeakExtractorRelationObjectPhrase(tokens []string) bool {
	if len(tokens) == 0 {
		return true
	}
	if len(tokens) > 8 {
		return true
	}
	if extractorWeakRelationObjectStartTokens.Contains(tokens[0]) {
		return true
	}
	return IsWeakExtractorEntityPhrase(tokens)
}

func IsLowValueExtractorNoteText(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return true
	}
	for _, prefix := range lowValueExtractorNotePrefixes {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	for _, fragment := range lowValueExtractorNoteFragments {
		if strings.Contains(text, fragment) {
			return true
		}
	}
	return false
}

func IsSafeFirstPersonExtractorContentVerb(token string) bool {
	return safeFirstPersonExtractorContentVerbs.Contains(token)
}

func ThirdPersonExtractorContentVerb(token string) (string, bool) {
	verb, ok := thirdPersonPresentExtractorContentVerbs[token]
	return verb, ok
}

func IsUnsupportedFirstPersonExtractorContentStart(tokens []string) bool {
	if len(tokens) == 0 {
		return false
	}
	return unsupportedFirstPersonExtractorContentStarts.Contains(strings.ToLower(tokens[0]))
}

func IsAbstractMadeRelationObjectPhrase(tokens []string) bool {
	if len(tokens) == 0 {
		return true
	}
	for _, token := range tokens {
		if abstractMadeRelationObjectTokens.Contains(strings.ToLower(token)) {
			return true
		}
	}
	for _, phrase := range abstractMadeRelationObjectPhrases {
		if hasTokenSequence(tokens, phrase) {
			return true
		}
	}
	return false
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

func hasTokenSequence(tokens []string, phrase []string) bool {
	if len(phrase) == 0 || len(tokens) < len(phrase) {
		return false
	}
	for i := 0; i <= len(tokens)-len(phrase); i++ {
		matched := true
		for j, part := range phrase {
			if strings.ToLower(tokens[i+j]) != part {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
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
