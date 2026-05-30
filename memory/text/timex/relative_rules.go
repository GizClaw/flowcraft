package timex

import "regexp"

type countedRelativeRule struct {
	pattern        *regexp.Regexp
	countGroups    []int
	unitGroups     []int
	direction      int
	directionGroup int
}

type countedRelativeMatch struct {
	text      string
	index     int
	count     string
	unit      string
	direction int
}

type directionalRelativeRule struct {
	pattern        *regexp.Regexp
	directionGroup int
	unitGroup      int
}

type directionalRelativeMatch struct {
	text      string
	index     int
	direction string
	unit      string
}

type lexicalRelativePhrase struct {
	text   string
	days   int
	months int
	years  int
	now    bool
}

type lexicalRelativePhraseGroup struct {
	locale  string
	phrases []lexicalRelativePhrase
}

var countedRelativeRules = []countedRelativeRule{
	{
		pattern:     regexp.MustCompile(`\b(\d+|one|two|three|four|five|six|seven|eight|nine|ten|a|an)\s+(days?|weeks?|weekends?|months?|years?)\s+ago\b`),
		countGroups: []int{1},
		unitGroups:  []int{2},
		direction:   -1,
	},
	{
		pattern:     regexp.MustCompile(`\b(?:in\s+(\d+|one|two|three|four|five|six|seven|eight|nine|ten|a|an)\s+(days?|weeks?|weekends?|months?|years?)|(\d+|one|two|three|four|five|six|seven|eight|nine|ten|a|an)\s+(days?|weeks?|weekends?|months?|years?)\s+from now)\b`),
		countGroups: []int{1, 3},
		unitGroups:  []int{2, 4},
		direction:   1,
	},
	{
		pattern:     regexp.MustCompile(`\bhace\s+(\d+|un|una|uno|dos|tres|cuatro|cinco|seis|siete|ocho|nueve|diez)\s+(fines?\s+de\s+semana|d[ií]as?|semanas?|mes(?:es)?|a[ñn]os?)\b`),
		countGroups: []int{1},
		unitGroups:  []int{2},
		direction:   -1,
	},
	{
		pattern:     regexp.MustCompile(`\b(?:en|dentro de)\s+(\d+|un|una|uno|dos|tres|cuatro|cinco|seis|siete|ocho|nueve|diez)\s+(fines?\s+de\s+semana|d[ií]as?|semanas?|mes(?:es)?|a[ñn]os?)\b`),
		countGroups: []int{1},
		unitGroups:  []int{2},
		direction:   1,
	},
	{
		pattern:     regexp.MustCompile(`\bil y a\s+(\d+|un|une|deux|trois|quatre|cinq|six|sept|huit|neuf|dix)\s+(week-?ends?|jours?|semaines?|mois|ans?|ann[ée]es?)\b`),
		countGroups: []int{1},
		unitGroups:  []int{2},
		direction:   -1,
	},
	{
		pattern:     regexp.MustCompile(`\bdans\s+(\d+|un|une|deux|trois|quatre|cinq|six|sept|huit|neuf|dix)\s+(week-?ends?|jours?|semaines?|mois|ans?|ann[ée]es?)\b`),
		countGroups: []int{1},
		unitGroups:  []int{2},
		direction:   1,
	},
	{
		pattern:        regexp.MustCompile(`([0-9]+|[一二两三四五六七八九十百千万零〇]+)个?(天|日|周末|周|星期|礼拜|月|年)(前|后)`),
		countGroups:    []int{1},
		unitGroups:     []int{2},
		directionGroup: 3,
	},
}

var relativeDirectionalRule = directionalRelativeRule{
	pattern:        regexp.MustCompile(`\b(next|last|this)\s+(day|week|weekend|month|year|monday|tuesday|wednesday|thursday|friday|saturday|sunday)\b`),
	directionGroup: 1,
	unitGroup:      2,
}

var lexicalRelativePhraseGroups = []lexicalRelativePhraseGroup{
	{
		locale: "en",
		phrases: []lexicalRelativePhrase{
			{text: "now", now: true},
			{text: "today"},
			{text: "tomorrow", days: 1},
			{text: "yesterday", days: -1},
			{text: "next week", days: 7},
			{text: "last week", days: -7},
			{text: "this week"},
			{text: "next month", months: 1},
			{text: "last month", months: -1},
			{text: "this month"},
			{text: "next year", years: 1},
			{text: "last year", years: -1},
			{text: "this year"},
		},
	},
	{
		locale: "es",
		phrases: []lexicalRelativePhrase{
			{text: "hoy"},
			{text: "mañana", days: 1},
			{text: "manana", days: 1},
			{text: "ayer", days: -1},
			{text: "la próxima semana", days: 7},
			{text: "la proxima semana", days: 7},
			{text: "la semana pasada", days: -7},
			{text: "el próximo mes", months: 1},
			{text: "el proximo mes", months: 1},
			{text: "el mes pasado", months: -1},
			{text: "el próximo año", years: 1},
			{text: "el proximo año", years: 1},
			{text: "el año pasado", years: -1},
		},
	},
	{
		locale: "fr",
		phrases: []lexicalRelativePhrase{
			{text: "aujourd hui"},
			{text: "demain", days: 1},
			{text: "hier", days: -1},
			{text: "la semaine prochaine", days: 7},
			{text: "la semaine dernière", days: -7},
			{text: "la semaine derniere", days: -7},
			{text: "le mois prochain", months: 1},
			{text: "le mois dernier", months: -1},
			{text: "l année prochaine", years: 1},
			{text: "l annee prochaine", years: 1},
			{text: "l année dernière", years: -1},
			{text: "l annee derniere", years: -1},
		},
	},
	{
		locale: "de",
		phrases: []lexicalRelativePhrase{
			{text: "heute"},
			{text: "morgen", days: 1},
			{text: "gestern", days: -1},
			{text: "nächste woche", days: 7},
			{text: "nachste woche", days: 7},
			{text: "letzte woche", days: -7},
			{text: "nächster monat", months: 1},
			{text: "nachster monat", months: 1},
			{text: "letzter monat", months: -1},
			{text: "nächstes jahr", years: 1},
			{text: "nachstes jahr", years: 1},
			{text: "letztes jahr", years: -1},
		},
	},
	{
		locale: "pt",
		phrases: []lexicalRelativePhrase{
			{text: "hoje"},
			{text: "amanhã", days: 1},
			{text: "amanha", days: 1},
			{text: "ontem", days: -1},
			{text: "próxima semana", days: 7},
			{text: "proxima semana", days: 7},
			{text: "semana passada", days: -7},
			{text: "próximo mês", months: 1},
			{text: "proximo mes", months: 1},
			{text: "mês passado", months: -1},
			{text: "mes passado", months: -1},
			{text: "próximo ano", years: 1},
			{text: "proximo ano", years: 1},
			{text: "ano passado", years: -1},
		},
	},
	{
		locale: "nl",
		phrases: []lexicalRelativePhrase{
			{text: "vandaag"},
			{text: "morgen", days: 1},
			{text: "gisteren", days: -1},
			{text: "volgende week", days: 7},
			{text: "vorige week", days: -7},
			{text: "volgende maand", months: 1},
			{text: "vorige maand", months: -1},
			{text: "volgend jaar", years: 1},
			{text: "vorig jaar", years: -1},
		},
	},
	{
		locale: "ru",
		phrases: []lexicalRelativePhrase{
			{text: "сегодня"},
			{text: "завтра", days: 1},
			{text: "вчера", days: -1},
			{text: "на следующей неделе", days: 7},
			{text: "на прошлой неделе", days: -7},
			{text: "в следующем месяце", months: 1},
			{text: "в прошлом месяце", months: -1},
			{text: "в следующем году", years: 1},
			{text: "в прошлом году", years: -1},
		},
	},
	{
		locale: "zh",
		phrases: []lexicalRelativePhrase{
			{text: "今天"},
			{text: "明天", days: 1},
			{text: "昨天", days: -1},
			{text: "后天", days: 2},
			{text: "前天", days: -2},
			{text: "下周", days: 7},
			{text: "上周", days: -7},
			{text: "下个月", months: 1},
			{text: "上个月", months: -1},
			{text: "明年", years: 1},
			{text: "去年", years: -1},
		},
	},
}

var lexicalRelativePhrases = flattenLexicalRelativePhraseGroups(lexicalRelativePhraseGroups)

func flattenLexicalRelativePhraseGroups(groups []lexicalRelativePhraseGroup) []lexicalRelativePhrase {
	var out []lexicalRelativePhrase
	for _, group := range groups {
		out = append(out, group.phrases...)
	}
	return out
}
