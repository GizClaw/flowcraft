package words

var commonGraphNouns = stringSet(
	"user", "users", "person", "people",
	"someone", "somebody", "anyone", "everyone",
	"thing", "things", "something", "anything",
	"place", "places", "somewhere", "anywhere",
	"time", "day", "week", "month", "year",
	"today", "tomorrow", "yesterday",
	"they", "them", "their", "it", "its",
	"he", "she", "we", "i", "you",
	"usuario", "persona", "personas", "alguien", "cualquiera",
	"cosa", "cosas", "lugar", "lugares", "tiempo", "día", "dia",
	"semana", "mes", "año", "ano", "hoy", "mañana", "manana", "ayer",
	"utilisateur", "personne", "personnes", "quelqu", "chose",
	"choses", "lieu", "endroit", "temps", "jour", "semaine", "mois",
	"année", "annee", "aujourd", "demain", "hier",
	"benutzer", "nutzer", "personen", "jemand", "ding",
	"dinge", "ort", "orte", "zeit", "tag", "woche", "monat",
	"jahr", "heute", "morgen", "gestern",
	"usuário", "pessoa", "pessoas", "alguém", "alguem",
	"coisa", "coisas", "mês", "hoje", "amanhã", "amanha", "ontem",
	"gebruiker", "persoon", "mensen", "iemand", "plek",
	"plaats", "tijd", "dag", "maand", "jaar", "vandaag", "gisteren",
	"пользователь", "человек", "люди", "кто-то", "кто", "вещь",
	"место", "время", "день", "неделя", "месяц", "год",
	"сегодня", "завтра", "вчера",
	"用户", "人", "某人", "大家", "东西", "事情", "地方",
	"时间", "天", "周", "星期", "月", "年", "今天", "明天", "昨天",
)

// IsCommonGraphNoun reports whether a canonical graph node is too generic to
// produce a useful entity edge.
func IsCommonGraphNoun(node string) bool {
	_, ok := commonGraphNouns[node]
	return ok
}

func stringSet(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}
